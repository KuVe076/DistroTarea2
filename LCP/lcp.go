package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	pb "DISTROTAREA2/proto"
	"github.com/streadway/amqp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	lcpPort                     = ":50051"
	rabbitMQURL                 = "amqp://guest:guest@rabbitmq:5672/"
	lcpEventsExchange           = "lcp_events_exchange"
	routingKeyLCPEvent          = "lcp.event"
	lcpRegistroEntrenadoresFile = "entrenadores_pequeno.json"
	idEntrenadorManual          = "MANUAL001" // ID para identificar al entrenador manual
)

type EntrenadorLCPData struct {
	ID         string `json:"id"`
	Nombre     string `json:"nombre"`
	Region     string `json:"region"`
	Ranking    int    `json:"ranking"`
	Estado     string `json:"estado"`
	Suspension int    `json:"suspension"`
}

type EventoLCPGeneral struct {
	TipoEvento string      `json:"tipo_evento"`
	Datos      interface{} `json:"datos"`
	Timestamp  string      `json:"timestamp"`
}

type DatosInscripcionEvento struct {
	EntrenadorID     string `json:"entrenador_id"`
	EntrenadorNombre string `json:"entrenador_nombre"`
	TorneoID         string `json:"torneo_id"`
	RegionTorneo     string `json:"region_torneo,omitempty"`
	MensajeAdicional string `json:"mensaje_adicional,omitempty"`
}

type DatosNuevoTorneoEvento struct {
	TorneoID      string `json:"torneo_id"`
	RegionTorneo  string `json:"region_torneo"`
	FechaCreacion string `json:"fecha_creacion"`
}

type lcpServer struct {
	pb.UnimplementedLigaPokemonServer
	mu                            sync.Mutex
	torneos                       map[string]*pb.Torneo
	inscripciones                 map[string]string
	registroEntrenadores          map[string]*EntrenadorLCPData
	rabbitMQChannel               *amqp.Channel
	entrenadoresEnEsperaPorTorneo map[string][]*EntrenadorLCPData
	gimnasioClientsByRegion      map[string]pb.GimnasioClient
	gimnasioAddressesByRegion    map[string]string
}

func cargarRegistroEntrenadoresLCP(filename string) (map[string]*EntrenadorLCPData, error) {
	data, errReadFile := os.ReadFile(filename)
	if errReadFile != nil {
		log.Printf("LCP WARN: No se pudo leer el archivo de registro '%s': %v.", filename, errReadFile)
		return make(map[string]*EntrenadorLCPData), nil
	}
	var entrenadoresJSON []EntrenadorLCPData
	errUnmarshal := json.Unmarshal(data, &entrenadoresJSON)
	if errUnmarshal != nil {
		return nil, fmt.Errorf("error decodificando JSON de registro de entrenadores '%s': %w", filename, errUnmarshal)
	}
	registro := make(map[string]*EntrenadorLCPData)
	for i := range entrenadoresJSON {
		entrenador := entrenadoresJSON[i]
		registro[entrenador.ID] = &entrenador
	}
	log.Printf("LCP: %d entrenadores cargados al registro interno desde %s.", len(registro), filename)
	return registro, nil
}

func nuevoLCPServer(connRabbit *amqp.Connection) (*lcpServer, error) {
	var ch *amqp.Channel
	var err error
	ch, err = connRabbit.Channel()
	if err != nil {
		return nil, fmt.Errorf("LCP: falló abrir canal Rabbit: %w", err)
	}
	err = ch.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
	if err != nil {
		ch.Close()
		return nil, fmt.Errorf("LCP: falló declarar exchange: %w", err)
	}
	registro, errCarga := cargarRegistroEntrenadoresLCP(lcpRegistroEntrenadoresFile)
	if errCarga != nil {
		log.Printf("LCP ERROR: Falló cargar registro: %v", errCarga)
	}
	s := &lcpServer{
		torneos:                       make(map[string]*pb.Torneo),
		inscripciones:                 make(map[string]string),
		registroEntrenadores:          registro,
		rabbitMQChannel:               ch,
		entrenadoresEnEsperaPorTorneo: make(map[string][]*EntrenadorLCPData),
		gimnasioClientsByRegion:      make(map[string]pb.GimnasioClient),
		gimnasioAddressesByRegion: map[string]string{
			"Kanto": "gimnasios:50052", 
			"Johto": "gimnasios:50053", 
			"Hoenn": "gimnasios:50054",
			"Sinnoh": "gimnasios:50055", 
			"Teselia": "gimnasios:50056", 
			"Kalos": "gimnasios:50057",
			"Alola": "gimnasios:50058", 
			"Galar": "gimnasios:50059", 
			"Paldea": "gimnasios:50060",
		},
	}
	s.conectarAGimnasios()
	s.inicializarTorneosDeEjemplo()
	return s, nil
}

func (s *lcpServer) conectarAGimnasios() {
	for region, addr := range s.gimnasioAddressesByRegion {
		log.Printf("LCP: Intentando conectar al gimnasio de %s en %s...", region, addr)
		ctxConnect, cancelConnect := context.WithTimeout(context.Background(), 10*time.Second)
		var conn *grpc.ClientConn
		var errDial error
		conn, errDial = grpc.DialContext(ctxConnect, addr, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		cancelConnect()
		if errDial != nil {
			log.Printf("LCP WARN: Falló conectar Gimnasio %s en %s: %v.", region, addr, errDial)
			continue
		}
		s.gimnasioClientsByRegion[region] = pb.NewGimnasioClient(conn)
		log.Printf("LCP: Conectado a Gimnasio de %s.", region)
	}
}

func (s *lcpServer) inicializarTorneosDeEjemplo() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var regionesConGimnasio []string
	for region := range s.gimnasioClientsByRegion {
		regionesConGimnasio = append(regionesConGimnasio, region)
	}
	if len(regionesConGimnasio) == 0 {
		log.Printf("LCP WARN: No hay gimnasios conectados. No se crean torneos.")
		return
	}
	numTorneosACrear := 5
	nextTorneoIDCounter := 1
	for i := 0; i < numTorneosACrear; i++ {
		id := fmt.Sprintf("TORNEO%03d", nextTorneoIDCounter)
		nextTorneoIDCounter++
		region := regionesConGimnasio[rand.Intn(len(regionesConGimnasio))]
		s.torneos[id] = &pb.Torneo{Id: id, Region: region}
	}
	log.Printf("LCP: %d torneos inicializados.", len(s.torneos))
}

func (s *lcpServer) publicarEventoGeneralASNP(tipoEvento string, datos interface{}) {
	evento := EventoLCPGeneral{TipoEvento: tipoEvento, Datos: datos, Timestamp: time.Now().Format(time.RFC3339)}
	var cuerpo []byte
	var errMarshal error
	cuerpo, errMarshal = json.Marshal(evento)
	if errMarshal != nil {
		log.Printf("LCP: Error serializar '%s' SNP: %v", tipoEvento, errMarshal)
		return
	}
	var errPublish error
	errPublish = s.rabbitMQChannel.Publish(lcpEventsExchange, routingKeyLCPEvent, false, false, amqp.Publishing{ContentType: "application/json", Body: cuerpo})
	if errPublish != nil {
		log.Printf("LCP: Error publicar '%s' SNP: %v", tipoEvento, errPublish)
	} else {
		log.Printf("LCP: Evento '%s' publicado a SNP.", tipoEvento)
	}
}

func (s *lcpServer) ConsultarTorneosDisponibles(ctx context.Context, req *pb.ConsultaTorneosReq) (*pb.ListaTorneosResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Solicitud torneos de '%s'", req.GetSolicitanteInfo())
	var lista []*pb.Torneo
	for _, t := range s.torneos {
		lista = append(lista, t)
	}
	return &pb.ListaTorneosResp{Torneos: lista}, nil
}

func (s *lcpServer) InscribirEnTorneo(ctx context.Context, req *pb.InscripcionTorneoReq) (*pb.ResultadoInscripcionResp, error) {
	s.mu.Lock() // Bloqueo general para la operación

	eID := req.GetEntrenadorId()
	tID := req.GetTorneoId()
	eNombre := req.GetEntrenadorNombre()
	eRegionSolicitud := req.GetEntrenadorRegion()

	log.Printf("LCP: Intento de inscripción: IDEntrenador '%s' (%s, Región Solicitud: %s) a TorneoID '%s'",
		eID, eNombre, eRegionSolicitud, tID)

	entrenadorData, existeEnRegistro := s.registroEntrenadores[eID]
	if !existeEnRegistro {
		entrenadorData = &EntrenadorLCPData{ID: eID, Nombre: eNombre, Region: eRegionSolicitud, Ranking: 1500, Estado: "Activo", Suspension: 0}
		s.registroEntrenadores[eID] = entrenadorData
		log.Printf("LCP: Entrenador ID '%s' (%s) creado en registro como Activo en región %s.", eID, eNombre, eRegionSolicitud)
	}
	regionEntrenadorVerificada := entrenadorData.Region

	// Validaciones
	if entrenadorData.Estado == "Expulsado" {
		s.mu.Unlock()
		msg := "Estás Expulsado permanentemente y no puedes inscribirte."
		log.Printf("LCP: RECHAZADO (Expulsado) - Entrenador '%s'. %s", eNombre, msg)
		datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: "Expulsado"}
		s.publicarEventoGeneralASNP("inscripcion_rechazada_expulsado", datosEv)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_ENTRENADOR_EXPULSADO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}
	if entrenadorData.Estado == "Suspendido" {
		entrenadorData.Suspension--
		log.Printf("LCP: Suspensión de '%s' decrementada a %d.", eNombre, entrenadorData.Suspension)
		if entrenadorData.Suspension == 0 {
			entrenadorData.Estado = "Activo"
			log.Printf("LCP: Entrenador '%s' ahora está Activo tras cumplir suspensión.", eNombre)
		} else {
			s.mu.Unlock()
			msg := fmt.Sprintf("Intento de inscripción rechazado. Sigues suspendido por %d torneo(s) más.", entrenadorData.Suspension)
			log.Printf("LCP: RECHAZADO (Sigue Suspendido) - Entrenador '%s'. %s", eNombre, msg)
			datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: fmt.Sprintf("Suspensión restante: %d", entrenadorData.Suspension)}
			s.publicarEventoGeneralASNP("inscripcion_rechazada_suspension", datosEv)
			return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_SUSPENDIDO, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension), NuevoEstadoEntrenador: entrenadorData.Estado}, nil
		}
	}
	torneoInfo := s.torneos[tID]

	if torneoInfo.Region != regionEntrenadorVerificada {
		s.mu.Unlock()
		msg := fmt.Sprintf("No puedes inscribirte en el torneo '%s' (Región: %s) porque tu región es '%s'.", tID, torneoInfo.Region, regionEntrenadorVerificada)
		log.Printf("LCP: RECHAZADO (Región Incorrecta) - Entrenador '%s'. %s", eNombre, msg)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_TORNEO_REGION_INCORRECTA, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}
	torneoActualID, estaInscrito := s.inscripciones[eID]
	if estaInscrito {
		if torneoActualID == tID {
			s.mu.Unlock()
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Ya estabas inscrito en este torneo.", TorneoIdConfirmado: tID, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
		}
		s.mu.Unlock()
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: "Ya estás inscrito en otro torneo.", RazonRechazo: pb.RazonRechazo_RECHAZO_YA_INSCRITO_EN_OTRO_TORNEO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}

	// Inscripción exitosa a LCP
	s.inscripciones[eID] = tID
	entrenadorData.Estado = "Activo"; entrenadorData.Suspension = 0
	log.Printf("LCP: ÉXITO - '%s' inscrito en Torneo %s.", eNombre, tID)
	datosEvInsc := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: torneoInfo.Id, RegionTorneo: torneoInfo.Region, MensajeAdicional: "Confirmada."}
	s.publicarEventoGeneralASNP("inscripcion_exitosa", datosEvInsc)

	esEsteEntrenadorManual := (eID == idEntrenadorManual)

	if esEsteEntrenadorManual {
		log.Printf("LCP: Entrenador Manual '%s' inscrito. Buscando oponente automático para torneo %s...", eNombre, tID)
		var oponenteAutomatico *EntrenadorLCPData = nil
		var indiceOponente = -1
		for i, esperando := range s.entrenadoresEnEsperaPorTorneo[tID] { // Buscar en la cola del torneo actual
			if esperando.ID != idEntrenadorManual { // Asegurarse que el oponente sea automático
				oponenteAutomatico = esperando
				indiceOponente = i
				break
			}
		}
		if oponenteAutomatico != nil {
			s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID][:indiceOponente], s.entrenadoresEnEsperaPorTorneo[tID][indiceOponente+1:]...)
			log.Printf("LCP: Emparejamiento para Manual '%s' vs Auto '%s' en torneo %s.", entrenadorData.Nombre, oponenteAutomatico.Nombre, tID)
			s.mu.Unlock() // Desbloquear ANTES de llamada síncrona
			s.asignarCombateAGimnasio(torneoInfo, entrenadorData, oponenteAutomatico)
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción exitosa y emparejamiento con automático asignado.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
		} else {
			s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData) // Manual queda en espera
			log.Printf("LCP: Entrenador Manual '%s' añadido a espera del torneo %s. No hay automáticos.", eNombre, tID)
			s.mu.Unlock()
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción exitosa. Esperando oponente automático.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
		}
	} else { // Entrenador automático
		s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData)
		log.Printf("LCP: Automático '%s' añadido a espera torneo %s. Total esperando: %d", eNombre, tID, len(s.entrenadoresEnEsperaPorTorneo[tID]))
		s.mu.Unlock()
		return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción exitosa. Automático en espera.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
	}
}

func (s *lcpServer) asignarCombateAGimnasio(torneo *pb.Torneo, e1, e2 *EntrenadorLCPData) {
	log.Print("ESTEBAN WEKO")
	regionDelCombate := torneo.Region
	gimnasioCliente, ok := s.gimnasioClientsByRegion[regionDelCombate]
	if !ok {
		log.Printf("LCP ERROR: No cliente gimnasio %s. Combate (%s vs %s) no asignado.", regionDelCombate, e1.Nombre, e2.Nombre)
		s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock()
		log.Printf("LCP: Entrenadores %s y %s devueltos a espera torneo %s (gimnasio no disponible).", e1.Nombre, e2.Nombre, torneo.Id)
		return
	}
	timestampNano := time.Now().UnixNano(); randomSuffix := rand.Intn(10000)
	combateID := fmt.Sprintf("combate-%d-%d", timestampNano, randomSuffix)
	log.Printf("LCP: Asignando combate ID %s (T:%s, %s vs %s) a gimnasio %s", combateID, torneo.Id, e1.Nombre, e2.Nombre, regionDelCombate)
	
	reqAsignacion := &pb.AsignarCombateRequest{
		CombateId: combateID, TorneoId: torneo.Id,
		Entrenador_1: &pb.EntrenadorInfo{Id: e1.ID, Name: e1.Nombre, Ranking: int32(e1.Ranking)},
		Entrenador_2: &pb.EntrenadorInfo{Id: e2.ID, Name: e2.Nombre, Ranking: int32(e2.Ranking)},
		Region:       regionDelCombate, // Coincide con 'Region' en el proto AsignarCombateRequest
	}
	ctxAsignacion, cancelAsignacion := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelAsignacion()
	
	var respAsignacion *pb.AsignarCombateResponse; var errAsignar error
	respAsignacion, errAsignar = gimnasioCliente.AsignarCombate(ctxAsignacion, reqAsignacion)
	if errAsignar != nil {
		log.Printf("LCP ERROR: Falló gRPC AsignarCombate (Gym:%s, Cmb:%s): %v", regionDelCombate, combateID, errAsignar)
		s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock()
		log.Printf("LCP: Entrenadores %s y %s devueltos a espera torneo %s (error asignación).", e1.Nombre, e2.Nombre, torneo.Id)
		return
	}
	if respAsignacion.GetAceptado() {
		log.Printf("LCP: Gimnasio %s aceptó combate %s. Msg: %s", regionDelCombate, combateID, respAsignacion.GetMensaje())
	} else {
		log.Printf("LCP WARN: Gimnasio %s RECHAZÓ combate %s. Msg: %s", regionDelCombate, combateID, respAsignacion.GetMensaje())
		s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock()
		log.Printf("LCP: Entrenadores %s y %s devueltos a espera torneo %s (rechazo gimnasio).", e1.Nombre, e2.Nombre, torneo.Id)
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("LCP: Iniciando servidor...")
	var err error
	var connRabbit *amqp.Connection
	connRabbit, err = amqp.Dial(rabbitMQURL)
	if err != nil { log.Fatalf("LCP: Falló conectar RabbitMQ: %s", err) }
	defer connRabbit.Close()
	log.Println("LCP: Conectado a RabbitMQ.")

	var servidorLCP *lcpServer
	servidorLCP, err = nuevoLCPServer(connRabbit)
	if err != nil { log.Fatalf("LCP: Falló crear instancia del servidor: %v", err) }
	if servidorLCP.rabbitMQChannel != nil {
		defer servidorLCP.rabbitMQChannel.Close()
	}

	var lis net.Listener
	lis, err = net.Listen("tcp", lcpPort)
	if err != nil { log.Fatalf("LCP: Falló al escuchar en %s: %v", lcpPort, err) }

	grpcServer := grpc.NewServer()
	pb.RegisterLigaPokemonServer(grpcServer, servidorLCP)
	log.Printf("LCP: Servidor gRPC escuchando en %s", lis.Addr())
	err = grpcServer.Serve(lis)
	if err != nil { log.Fatalf("LCP: Falló al servir gRPC: %v", err) }
}