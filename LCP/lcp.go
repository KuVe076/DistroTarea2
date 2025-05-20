package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os" // Para cargar el JSON de entrenadores para el registro de LCP
	"sync"
	"time"

	pb "DISTROTAREA2/proto"
	"github.com/streadway/amqp"
	"google.golang.org/grpc"
)

const (
	lcpPort                = ":50051"
	rabbitMQURL = "amqp://guest:guest@rabbitmq:5672/"
	lcpEventsExchange      = "lcp_events_exchange"          // LCP publica aquí
	routingKeyLCPEvent     = "lcp.event.inscripcion"        // Routing key para eventos de LCP (SNP escuchará esto)
	lcpRegistroEntrenadoresFile = "entrenadores_pequeno.json" // LCP usa este archivo para conocer a los entrenadores
)

// Estructura para el servidor LCP
type lcpServer struct {
	pb.UnimplementedLigaPokemonServer
	mu                   sync.Mutex
	torneos              map[string]*pb.Torneo           // [torneoID] -> Torneo
	inscripciones        map[string]string               // [entrenadorID] -> torneoID
	registroEntrenadores map[string]*EntrenadorLCPData   // [entrenadorID] -> Datos del entrenador
	rabbitMQChannel      *amqp.Channel
}

// EntrenadorLCPData (como en el PDF pág 4)
type EntrenadorLCPData struct {
	ID         string `json:"id"` // Tags para coincidir con el JSON que LCP podría cargar
	Nombre     string `json:"nombre"`
	Region     string `json:"region"`
	Ranking    int    `json:"ranking"`
	Estado     string `json:"estado"`    // "Activo", "Suspendido", "Expulsado"
	Suspension int    `json:"suspension"`// Número de TORNEOS de suspensión (no días)
}

// Mensaje que LCP envía a SNP
type EventoInscripcionLCP struct {
	TipoEvento       string `json:"tipo_evento"` // "inscripcion_exitosa", "inscripcion_rechazada_suspension"
	EntrenadorID     string `json:"entrenador_id"`
	EntrenadorNombre string `json:"entrenador_nombre"`
	TorneoID         string `json:"torneo_id,omitempty"` // Si la inscripción fue a un torneo
	RegionTorneo     string `json:"region_torneo,omitempty"`
	MensajeAdicional string `json:"mensaje_adicional,omitempty"` // ej: "Suspensión restante: X"
	Timestamp        string `json:"timestamp"`
}


func cargarRegistroEntrenadoresLCP(filename string) (map[string]*EntrenadorLCPData, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		// Si el archivo no existe, LCP podría empezar sin un registro pre-cargado
		// o podrías decidir que es un error fatal. Por ahora, solo advertimos.
		log.Printf("LCP WARN: No se pudo leer el archivo de registro de entrenadores '%s': %v. LCP iniciará sin registro pre-cargado.", filename, err)
		return make(map[string]*EntrenadorLCPData), nil // Devuelve mapa vacío
	}
	var entrenadoresJSON []EntrenadorLCPData // El JSON es un array de estos structs
	err = json.Unmarshal(data, &entrenadoresJSON)
	if err != nil {
		return nil, fmt.Errorf("error decodificando JSON de registro de entrenadores '%s': %w", filename, err)
	}
	registro := make(map[string]*EntrenadorLCPData)
	for i := range entrenadoresJSON {
		entrenador := entrenadoresJSON[i] // Copiar para evitar problemas con el puntero en el bucle si se usa &entrenador más adelante
		registro[entrenador.ID] = &entrenador
	}
	log.Printf("LCP: %d entrenadores cargados al registro interno desde %s.", len(registro), filename)
	return registro, nil
}

func nuevoLCPServer(connRabbit *amqp.Connection) (*lcpServer, error) {
	var ch *amqp.Channel
	var err error
	ch, err = connRabbit.Channel()
	if err != nil { return nil, fmt.Errorf("LCP: falló al abrir canal RabbitMQ: %w", err) }

	err = ch.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
	if err != nil { return nil, fmt.Errorf("LCP: falló al declarar exchange '%s': %w", lcpEventsExchange, err) }

	registro, errCarga := cargarRegistroEntrenadoresLCP(lcpRegistroEntrenadoresFile)
	if errCarga != nil {
		// Podrías decidir si es fatal o no. Por ahora, LCP puede funcionar sin un registro precargado,
		// y los entrenadores se "auto-registrarían" al primer intento de inscripción.
		log.Printf("LCP ERROR: Falló al cargar el registro inicial de entrenadores: %v", errCarga)
	}

	s := &lcpServer{
		torneos:              make(map[string]*pb.Torneo),
		inscripciones:        make(map[string]string),
		registroEntrenadores: registro, // Usar el registro cargado (o vacío si falló la carga)
		rabbitMQChannel:      ch,
	}
	s.inicializarTorneosDeEjemplo()
	return s, nil
}

func (s *lcpServer) inicializarTorneosDeEjemplo() {
	s.mu.Lock()
	defer s.mu.Unlock()
	regiones := []string{"Kanto", "Johto", "Hoenn", "Sinnoh"}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("TORNEO%02d", i+1)
		s.torneos[id] = &pb.Torneo{Id: id, Region: regiones[rand.Intn(len(regiones))]}
	}
	log.Printf("LCP: %d torneos de ejemplo inicializados.", len(s.torneos))
}

func (s *lcpServer) ConsultarTorneosDisponibles(ctx context.Context, req *pb.ConsultaTorneosReq) (*pb.ListaTorneosResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Solicitud de torneos de '%s'", req.GetSolicitanteInfo())
	var lista []*pb.Torneo
	for _, t := range s.torneos { lista = append(lista, t) }
	return &pb.ListaTorneosResp{Torneos: lista}, nil
}

func (s *lcpServer) InscribirEnTorneo(ctx context.Context, req *pb.InscripcionTorneoReq) (*pb.ResultadoInscripcionResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	eID := req.GetEntrenadorId()
	tID := req.GetTorneoId()
	eNombre := req.GetEntrenadorNombre() // Usar el nombre de la solicitud

	log.Printf("LCP: Intento de inscripción: IDEntrenador '%s' (%s) a TorneoID '%s'", eID, eNombre, tID)

	// Obtener datos del entrenador del registro de LCP.
	// Si no existe, se crea uno por defecto "Activo" (según discusión previa, pero el PDF implica que LCP ya los conoce).
	// Vamos a asumir que si no está, LCP lo crea en estado Activo por simplicidad para este laboratorio.
	entrenadorData, existeEnRegistro := s.registroEntrenadores[eID]
	if !existeEnRegistro {
		log.Printf("LCP: Entrenador ID '%s' (%s) no en registro. Creando como Activo.", eID, eNombre)
		entrenadorData = &EntrenadorLCPData{ID: eID, Nombre: eNombre, Region: req.GetEntrenadorRegion(), Ranking: 1500, Estado: "Activo", Suspension: 0}
		s.registroEntrenadores[eID] = entrenadorData
	}

	// 1. Verificar si está Expulsado
	if entrenadorData.Estado == "Expulsado" {
		msg := "Estás Expulsado permanentemente y no puedes inscribirte."
		log.Printf("LCP: RECHAZADO (Expulsado) - Entrenador '%s'. %s", eNombre, msg)
		// Podría notificarse al SNP también si es un intento de un expulsado.
		s.publicarEventoInscripcionASNP("inscripcion_rechazada_expulsado", entrenadorData, tID, msg)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_ENTRENADOR_EXPULSADO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}

	// 2. Aplicar Regla de Suspensión (PDF pág 5)
	if entrenadorData.Estado == "Suspendido" {
		log.Printf("LCP: Entrenador '%s' (%s) está Suspendido. Suspensión actual: %d torneos.", eNombre, eID, entrenadorData.Suspension)
		entrenadorData.Suspension-- // Se decrementa su contador de Suspensión en 1.
		log.Printf("LCP: Suspensión de '%s' decrementada a %d.", eNombre, entrenadorData.Suspension)

		if entrenadorData.Suspension == 0 {
			entrenadorData.Estado = "Activo" // Si Suspensión == 0, su estado cambia a Activo
			log.Printf("LCP: Entrenador '%s' ahora está Activo tras cumplir suspensión.", eNombre)
			// Puede continuar para intentar inscribirse en ESTA MISMA LLAMADA.
		} else {
			// Si Suspensión > 0, se rechaza la inscripción y se notifica al SNP.
			msg := fmt.Sprintf("Intento de inscripción rechazado. Sigues suspendido por %d torneo(s) más.", entrenadorData.Suspension)
			log.Printf("LCP: RECHAZADO (Sigue Suspendido) - Entrenador '%s'. %s", eNombre, msg)
			s.publicarEventoInscripcionASNP("inscripcion_rechazada_suspension", entrenadorData, tID, fmt.Sprintf("Suspensión restante: %d", entrenadorData.Suspension))
			return &pb.ResultadoInscripcionResp{
				Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_SUSPENDIDO,
				NuevaSuspensionEntrenador: int32(entrenadorData.Suspension), NuevoEstadoEntrenador: entrenadorData.Estado,
			}, nil
		}
	}
	// Si llegó aquí, el entrenador está "Activo" (ya sea porque lo estaba, o porque acaba de cumplir su suspensión).

	// 3. Verificar si el torneo existe
	torneoInfo, existeTorneo := s.torneos[tID]
	if !existeTorneo {
		msg := fmt.Sprintf("El torneo con ID '%s' no existe.", tID)
		log.Printf("LCP: RECHAZADO (Torneo no existe) - Entrenador '%s'. %s", eNombre, msg)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_TORNEO_NO_EXISTE, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}

	// 4. Verificar si ya está inscrito en OTRO torneo (PDF pág 4: "Valida que el entrenador no esté inscrito en múltiples torneos simultáneamente")
	torneoActualID, estaInscrito := s.inscripciones[eID]
	if estaInscrito {
		if torneoActualID == tID { // Ya inscrito en ESTE torneo
			msg := "Ya estabas inscrito en este torneo."
			log.Printf("LCP: ACEPTADO (Ya inscrito en este torneo) - Entrenador '%s'.", eNombre)
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: msg, TorneoIdConfirmado: tID, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
		}
		// Inscrito en un torneo DIFERENTE
		msg := fmt.Sprintf("Ya estás inscrito en el torneo %s. No puedes inscribirte en múltiples torneos.", torneoActualID)
		log.Printf("LCP: RECHAZADO (Inscrito en otro torneo) - Entrenador '%s'. %s", eNombre, msg)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_YA_INSCRITO_EN_OTRO_TORNEO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}

	// Inscripción exitosa
	s.inscripciones[eID] = tID
	// El estado ya debería ser "Activo" y Suspensión 0 si llegó hasta aquí.
	entrenadorData.Estado = "Activo"
	entrenadorData.Suspension = 0

	msgExito := fmt.Sprintf("¡Inscripción exitosa al torneo %s (%s)!", torneoInfo.Id, torneoInfo.Region)
	log.Printf("LCP: ÉXITO - Entrenador '%s' inscrito en Torneo ID %s.", eNombre, tID)

	// Notificar al SNP sobre la inscripción exitosa
	s.publicarEventoInscripcionASNP("inscripcion_exitosa", entrenadorData, torneoInfo.Id, "Confirmada")

	return &pb.ResultadoInscripcionResp{
		Exito:             true,
		Mensaje:           msgExito,
		TorneoIdConfirmado: tID,
		NuevaSuspensionEntrenador: int32(entrenadorData.Suspension), // Debería ser 0
		NuevoEstadoEntrenador:     entrenadorData.Estado,         // Debería ser "Activo"
	}, nil
}

func (s *lcpServer) publicarEventoInscripcionASNP(tipoEvento string, eData *EntrenadorLCPData, torneoID, mensajeAdicional string) {
	regionTorneo := ""
	if torneo, ok := s.torneos[torneoID]; ok {
		regionTorneo = torneo.Region
	}

	evento := EventoInscripcionLCP{
		TipoEvento:       tipoEvento,
		EntrenadorID:     eData.ID,
		EntrenadorNombre: eData.Nombre,
		TorneoID:         torneoID,
		RegionTorneo:     regionTorneo,
		MensajeAdicional: mensajeAdicional,
		Timestamp:        time.Now().Format(time.RFC3339),
	}
	cuerpo, err := json.Marshal(evento)
	if err != nil { log.Printf("LCP: Error al serializar evento para SNP: %v", err); return }

	err = s.rabbitMQChannel.Publish(
		lcpEventsExchange,  // exchange
		routingKeyLCPEvent, // routing key
		false,              // mandatory
		false,              // immediate
		amqp.Publishing{ContentType: "application/json", Body: cuerpo})
	if err != nil { log.Printf("LCP: Error al publicar evento '%s' para SNP: %v", tipoEvento, err) } else {
		log.Printf("LCP: Evento '%s' publicado a SNP para Entrenador %s.", tipoEvento, eData.Nombre)
	}
}

func main() {
	rand.Seed(time.Now().UnixNano()); log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("LCP: Iniciando servidor...")
	var err error // Variable de error reutilizable

	var connRabbit *amqp.Connection
	connRabbit, err = amqp.Dial(rabbitMQURL)
	if err != nil { log.Fatalf("LCP: Falló conectar RabbitMQ: %s", err) }
	defer connRabbit.Close(); log.Println("LCP: Conectado a RabbitMQ.")

	var servidorLCP *lcpServer
	servidorLCP, err = nuevoLCPServer(connRabbit)
	if err != nil { log.Fatalf("LCP: Falló crear instancia del servidor: %v", err) }
	defer servidorLCP.rabbitMQChannel.Close()

	var lis net.Listener
	lis, err = net.Listen("tcp", lcpPort)
	if err != nil { log.Fatalf("LCP: Falló al escuchar en %s: %v", lcpPort, err) }

	grpcServer := grpc.NewServer()
	pb.RegisterLigaPokemonServer(grpcServer, servidorLCP) // Usa el nombre de servicio del proto
	log.Printf("LCP: Servidor gRPC escuchando en %s", lis.Addr())
	err = grpcServer.Serve(lis)
	if err != nil { log.Fatalf("LCP: Falló al servir gRPC: %v", err) }
}