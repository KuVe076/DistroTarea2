package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	pb "DISTROTAREA2/proto" // Asegúrate que esta ruta es correcta
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
	idEntrenadorManual          = "MANUAL001"
	intervaloGeneracionTorneo   = 25 * time.Second
	maximoTorneosActivos        = 15
	torneosIniciales            = 5

	cdpPublicaResultadosExchange = "cdp_validados_exchange"
	lcpConsumeResultadosQueue    = "lcp_consume_resultados_cdp_queue"
	routingKeyResultadosDesdeCDP = "resultado.validado.lcp"
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

type DatosCambioRanking struct {
	EntrenadorID     string `json:"entrenador_id"`
	EntrenadorNombre string `json:"entrenador_nombre"`
	RankingAnterior  int    `json:"ranking_anterior"`
	RankingNuevo     int    `json:"ranking_nuevo"`
	ResultadoCombate string `json:"resultado_combate"`
	OponenteNombre   string `json:"oponente_nombre"`
	TorneoID         string `json:"torneo_id"`
	CombateID        string `json:"combate_id"`
}

type ResultadoCombateDesdeCDP struct {
	TorneoID          string `json:"torneo_id"`
	CombateID         string `json:"combate_id"`
	IDEntrenador1     string `json:"id_entrenador_1"`
	NombreEntrenador1 string `json:"nombre_entrenador_1"`
	IDEntrenador2     string `json:"id_entrenador_2"`
	NombreEntrenador2 string `json:"nombre_entrenador_2"`
	IDWinner          string `json:"id_winner"`
	NombreWinner      string `json:"nombre_winner"`
	Fecha             string `json:"fecha"`
	TipoMensaje       string `json:"tipo_mensaje"`
}

type lcpServer struct {
	pb.UnimplementedLigaPokemonServer
	mu                            sync.Mutex
	torneos                       map[string]*pb.Torneo
	nextTorneoCounter             int
	inscripciones                 map[string]string
	registroEntrenadores          map[string]*EntrenadorLCPData
	rabbitMQChannelPublicacion    *amqp.Channel
	rabbitMQChannelConsumo        *amqp.Channel
	entrenadoresEnEsperaPorTorneo map[string][]*EntrenadorLCPData
	gimnasioClientsByRegion      map[string]pb.GimnasioClient
	gimnasioAddressesByRegion    map[string]string
	shutdownChan                  chan struct{}
}

func cargarRegistroEntrenadoresLCP(filename string) (map[string]*EntrenadorLCPData, error) {
	data, errReadFile := os.ReadFile(filename)
	if errReadFile != nil {
		log.Printf("LCP WARN: No se pudo leer el archivo de registro '%s': %v. LCP iniciará sin registro pre-cargado.", filename, errReadFile)
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
	var chPub *amqp.Channel
	var errPub error
	chPub, errPub = connRabbit.Channel()
	if errPub != nil {
		return nil, fmt.Errorf("LCP: falló abrir canal Rabbit para publicación: %w", errPub)
	}
	errPub = chPub.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
	if errPub != nil {
		chPub.Close()
		return nil, fmt.Errorf("LCP: falló declarar exchange de publicación: %w", errPub)
	}

	var chConsume *amqp.Channel
	var errConsume error
	chConsume, errConsume = connRabbit.Channel()
	if errConsume != nil {
		chPub.Close() // Cerrar canal de publicación si el de consumo falla
		return nil, fmt.Errorf("LCP: falló al abrir canal Rabbit de consumo: %w", errConsume)
	}
	errConsume = chConsume.ExchangeDeclare(cdpPublicaResultadosExchange, "direct", true, false, false, false, nil)
	if errConsume != nil {
		chPub.Close(); chConsume.Close()
		return nil, fmt.Errorf("LCP: falló al declarar exchange para consumo CDP: %w", errConsume)
	}
	var q amqp.Queue
	q, errConsume = chConsume.QueueDeclare(lcpConsumeResultadosQueue, true, false, false, false, nil)
	if errConsume != nil {
		chPub.Close(); chConsume.Close()
		return nil, fmt.Errorf("LCP: falló al declarar cola para consumo CDP: %w", errConsume)
	}
	errConsume = chConsume.QueueBind(q.Name, routingKeyResultadosDesdeCDP, cdpPublicaResultadosExchange, false, nil)
	if errConsume != nil {
		chPub.Close(); chConsume.Close()
		return nil, fmt.Errorf("LCP: falló al enlazar cola para consumo CDP: %w", errConsume)
	}

	registro, errCarga := cargarRegistroEntrenadoresLCP(lcpRegistroEntrenadoresFile)
	if errCarga != nil {
		log.Printf("LCP ERROR: Falló cargar registro: %v", errCarga)
	}

	s := &lcpServer{
		torneos:                       make(map[string]*pb.Torneo),
		nextTorneoCounter:             1,
		inscripciones:                 make(map[string]string),
		registroEntrenadores:          registro,
		rabbitMQChannelPublicacion:    chPub,
		rabbitMQChannelConsumo:        chConsume,
		entrenadoresEnEsperaPorTorneo: make(map[string][]*EntrenadorLCPData),
		gimnasioClientsByRegion:      make(map[string]pb.GimnasioClient),
		gimnasioAddressesByRegion: map[string]string{
			"Kanto": "gimnasios:50052", "Johto": "gimnasios:50053", "Hoenn": "gimnasios:50054",
			"Sinnoh": "gimnasios:50055", "Teselia": "gimnasios:50056", "Kalos": "gimnasios:50057",
			"Alola": "gimnasios:50058", "Galar": "gimnasios:50059", "Paldea": "gimnasios:50060",
		},
		shutdownChan: make(chan struct{}),
	}
	s.conectarAGimnasios()
	s.generarTorneosIniciales()
	go s.generadorDeTorneosPeriodico()
	go s.consumirResultadosCDP()
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

func (s *lcpServer) generarUnNuevoTorneo(notificarSNP bool) *pb.Torneo {
	// Esta función es llamada con el lock de s.mu YA ADQUIRIDO
	var regionesConGimnasioConectado []string
	for region := range s.gimnasioClientsByRegion {
		regionesConGimnasioConectado = append(regionesConGimnasioConectado, region)
	}
	if len(regionesConGimnasioConectado) == 0 {
		log.Printf("LCP: No hay gimnasios conectados, no se puede generar torneo.")
		return nil
	}
	if len(s.torneos) >= maximoTorneosActivos {
		log.Printf("LCP: Máximo de %d torneos activos alcanzado.", maximoTorneosActivos)
		return nil
	}
	nuevoID := fmt.Sprintf("TORNEO%03d", s.nextTorneoCounter)
	s.nextTorneoCounter++
	regionAsignada := regionesConGimnasioConectado[rand.Intn(len(regionesConGimnasioConectado))]
	nuevoTorneo := &pb.Torneo{Id: nuevoID, Region: regionAsignada}
	s.torneos[nuevoID] = nuevoTorneo
	log.Printf("LCP: NUEVO TORNEO CREADO -> ID: %s, Región: %s. Total activos: %d", nuevoID, regionAsignada, len(s.torneos))
	if notificarSNP {
		datosEv := DatosNuevoTorneoEvento{TorneoID: nuevoID, RegionTorneo: regionAsignada, FechaCreacion: time.Now().Format("2006-01-02 15:04:05")}
		s.publicarEventoGeneralASNP("nuevo_torneo_disponible", datosEv)
	}
	return nuevoTorneo
}

func (s *lcpServer) generarTorneosIniciales() {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Generando %d torneos iniciales...", torneosIniciales)
	for i := 0; i < torneosIniciales; i++ {
		s.generarUnNuevoTorneo(true)
		if len(s.torneos) >= maximoTorneosActivos {
			log.Printf("LCP: Límite de torneos activos alcanzado durante la inicialización.")
			break
		}
	}
	log.Printf("LCP: %d torneos iniciales generados (o hasta alcanzar el máximo).", len(s.torneos))
}

func (s *lcpServer) generadorDeTorneosPeriodico() {
	log.Printf("LCP: Iniciando generador periódico de torneos (intervalo: %v).", intervaloGeneracionTorneo)
	ticker := time.NewTicker(intervaloGeneracionTorneo)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.mu.Lock()
			s.generarUnNuevoTorneo(true)
			s.mu.Unlock()
		case <-s.shutdownChan:
			log.Println("LCP: Deteniendo generador periódico de torneos.")
			return
		}
	}
}

func (s *lcpServer) publicarEventoGeneralASNP(tipoEvento string, datos interface{}) {
	evento := EventoLCPGeneral{TipoEvento: tipoEvento, Datos: datos, Timestamp: time.Now().Format(time.RFC3339)}
	var cuerpo []byte; var errMarshal error
	cuerpo, errMarshal = json.Marshal(evento)
	if errMarshal != nil {
		log.Printf("LCP: Error serializar '%s' SNP: %v", tipoEvento, errMarshal)
		return
	}
	if s.rabbitMQChannelPublicacion == nil {
		log.Printf("LCP ERROR: rabbitMQChannelPublicacion es nil al intentar publicar evento SNP.")
		return
	}
	var errPublish error
	errPublish = s.rabbitMQChannelPublicacion.Publish(lcpEventsExchange, routingKeyLCPEvent, false, false, amqp.Publishing{ContentType: "application/json", Body: cuerpo})
	if errPublish != nil {
		log.Printf("LCP: Error publicar '%s' SNP: %v", tipoEvento, errPublish)
	} else {
		log.Printf("LCP: Evento '%s' publicado a SNP.", tipoEvento)
	}
}

func (s *lcpServer) ConsultarTorneosDisponibles(ctx context.Context, req *pb.ConsultaTorneosReq) (*pb.ListaTorneosResp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Solicitud de torneos de '%s'", req.GetSolicitanteInfo())
	var lista []*pb.Torneo
	for _, t := range s.torneos {
		lista = append(lista, t)
	}
	return &pb.ListaTorneosResp{Torneos: lista}, nil
}

func (s *lcpServer) InscribirEnTorneo(ctx context.Context, req *pb.InscripcionTorneoReq) (*pb.ResultadoInscripcionResp, error) {
	s.mu.Lock()
	eID := req.GetEntrenadorId(); tID := req.GetTorneoId(); eNombre := req.GetEntrenadorNombre(); eRegionSolicitud := req.GetEntrenadorRegion()
	log.Printf("LCP: Intento inscripción: ID '%s' (%s, RegiónSol: %s) a TorneoID '%s'", eID, eNombre, eRegionSolicitud, tID)
	entrenadorData, existeEnRegistro := s.registroEntrenadores[eID]
	if !existeEnRegistro {
		entrenadorData = &EntrenadorLCPData{ID: eID, Nombre: eNombre, Region: eRegionSolicitud, Ranking: 1500, Estado: "Activo", Suspension: 0}
		s.registroEntrenadores[eID] = entrenadorData
		log.Printf("LCP: Entrenador ID '%s' (%s) creado.", eID, eNombre)
	}
	regionEntrenadorVerificada := entrenadorData.Region

	if entrenadorData.Estado == "Expulsado" {
		s.mu.Unlock(); msg := "Estás Expulsado."; log.Printf("LCP: RECHAZADO (Expulsado) - '%s'. %s", eNombre, msg)
		datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: "Expulsado"}
		s.publicarEventoGeneralASNP("inscripcion_rechazada_expulsado", datosEv)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_ENTRENADOR_EXPULSADO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}
	if entrenadorData.Estado == "Suspendido" {
		entrenadorData.Suspension--
		log.Printf("LCP: Suspensión de '%s' a %d.", eNombre, entrenadorData.Suspension)
		if entrenadorData.Suspension == 0 {
			entrenadorData.Estado = "Activo"
			log.Printf("LCP: '%s' ahora Activo.", eNombre)
		} else {
			s.mu.Unlock(); msg := fmt.Sprintf("Sigue suspendido por %d torneo(s).", entrenadorData.Suspension)
			log.Printf("LCP: RECHAZADO (Sigue Suspendido) - '%s'. %s", eNombre, msg)
			datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: fmt.Sprintf("Suspensión restante: %d", entrenadorData.Suspension)}
			s.publicarEventoGeneralASNP("inscripcion_rechazada_suspension", datosEv)
			return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_SUSPENDIDO, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension), NuevoEstadoEntrenador: entrenadorData.Estado}, nil
		}
	}
	torneoInfo := s.torneos[tID]

	if torneoInfo.Region != regionEntrenadorVerificada {
		s.mu.Unlock(); msg := fmt.Sprintf("Torneo '%s' es para %s, tu región es %s.", tID, torneoInfo.Region, regionEntrenadorVerificada)
		log.Printf("LCP: RECHAZADO (Región Incorrecta) - '%s'. %s", eNombre, msg)
		return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_TORNEO_REGION_INCORRECTA, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}
	torneoActualID, estaInscrito := s.inscripciones[eID]
	if estaInscrito {
		if torneoActualID == tID {
			s.mu.Unlock(); return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Ya inscrito.", TorneoIdConfirmado: tID, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
		}
		s.mu.Unlock(); return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: "Inscrito en otro.", RazonRechazo: pb.RazonRechazo_RECHAZO_YA_INSCRITO_EN_OTRO_TORNEO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
	}
	s.inscripciones[eID] = tID; entrenadorData.Estado = "Activo"; entrenadorData.Suspension = 0
	log.Printf("LCP: ÉXITO - '%s' inscrito en Torneo %s.", eNombre, tID)
	datosEvInsc := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: torneoInfo.Id, RegionTorneo: torneoInfo.Region, MensajeAdicional: "Confirmada."}
	s.publicarEventoGeneralASNP("inscripcion_exitosa", datosEvInsc)

	esEsteEntrenadorManual := (eID == idEntrenadorManual)
	if esEsteEntrenadorManual {
		log.Printf("LCP: Manual '%s' inscrito. Buscando oponente automático para torneo %s...", eNombre, tID)
		var oponenteAutomatico *EntrenadorLCPData = nil; var indiceOponente = -1
		for i, esperando := range s.entrenadoresEnEsperaPorTorneo[tID] {
			if esperando.ID != idEntrenadorManual { oponenteAutomatico = esperando; indiceOponente = i; break }
		}
		if oponenteAutomatico != nil {
			s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID][:indiceOponente], s.entrenadoresEnEsperaPorTorneo[tID][indiceOponente+1:]...)
			log.Printf("LCP: Emparejamiento Manual '%s' vs Auto '%s' en torneo %s.", entrenadorData.Nombre, oponenteAutomatico.Nombre, tID)
			s.mu.Unlock(); s.asignarCombateAGimnasio(torneoInfo, entrenadorData, oponenteAutomatico)
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción y emparejamiento con automático asignado.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
		} else {
			s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData)
			log.Printf("LCP: Manual '%s' añadido a espera torneo %s. No hay automáticos.", eNombre, tID)
			s.mu.Unlock()
			return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción. Esperando oponente automático.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
		}
	} else {
		s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData)
		log.Printf("LCP: Automático '%s' añadido a espera torneo %s. Total esperando: %d", eNombre, tID, len(s.entrenadoresEnEsperaPorTorneo[tID]))
		s.mu.Unlock()
		return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción. Automático en espera.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
	}
}

func (s *lcpServer) asignarCombateAGimnasio(torneo *pb.Torneo, e1, e2 *EntrenadorLCPData) {
	regionDelCombate := torneo.Region; gimnasioCliente, ok := s.gimnasioClientsByRegion[regionDelCombate]
	if !ok { log.Printf("LCP ERROR: No cliente gimnasio %s. Combate (%s vs %s) no asignado.", regionDelCombate, e1.Nombre, e2.Nombre); s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock(); return }
	tN := time.Now().UnixNano(); rS := rand.Intn(10000); cID := fmt.Sprintf("combate-%d-%d", tN, rS)
	log.Printf("LCP: Asignando combate ID %s (T:%s, %s vs %s) a gimnasio %s", cID, torneo.Id, e1.Nombre, e2.Nombre, regionDelCombate)
	reqA := &pb.AsignarCombateRequest{CombateId: cID, TorneoId: torneo.Id, Entrenador_1: &pb.EntrenadorInfo{Id: e1.ID, Name: e1.Nombre, Ranking: int32(e1.Ranking)}, Entrenador_2: &pb.EntrenadorInfo{Id: e2.ID, Name: e2.Nombre, Ranking: int32(e2.Ranking)}, Region: regionDelCombate}
	ctxA, cancelA := context.WithTimeout(context.Background(), 15*time.Second); defer cancelA()
	var respA *pb.AsignarCombateResponse; var errA error; respA, errA = gimnasioCliente.AsignarCombate(ctxA, reqA)
	if errA != nil { log.Printf("LCP ERROR: Falló gRPC AsignarCombate (Gym:%s, Cmb:%s): %v", regionDelCombate, cID, errA); s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock(); return }
	if respA.GetAceptado() { log.Printf("LCP: Gimnasio %s aceptó combate %s. Msg: %s", regionDelCombate, cID, respA.GetMensaje()) } else { log.Printf("LCP WARN: Gimnasio %s RECHAZÓ combate %s. Msg: %s", regionDelCombate, cID, respA.GetMensaje()); s.mu.Lock(); s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...); s.mu.Unlock() }
}

func (s *lcpServer) procesarResultadoDeCombate(res ResultadoCombateDesdeCDP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Procesando resultado combate ID %s (Torneo: %s). Ganador: %s (%s)", res.CombateID, res.TorneoID, res.NombreWinner, res.IDWinner)
	fmt.Printf("\nLCP: --- Resultado Final Combate %s (Torneo %s) ---\nGanador: %s (ID: %s)\n---------------------------------------------------\n\n", res.CombateID, res.TorneoID, res.NombreWinner, res.IDWinner)
	var idPerdedor, nombrePerdedor string
	if res.IDWinner == res.IDEntrenador1 { idPerdedor = res.IDEntrenador2; nombrePerdedor = res.NombreEntrenador2
	} else if res.IDWinner == res.IDEntrenador2 { idPerdedor = res.IDEntrenador1; nombrePerdedor = res.NombreEntrenador1
	} else { log.Printf("LCP ERROR: ID ganador '%s' no coincide con entrenadores (%s, %s) combate %s.", res.IDWinner, res.IDEntrenador1, res.IDEntrenador2, res.CombateID); return }
	
	cambioRankingVictoria := 50; cambioRankingDerrota := -30
	if ganadorData, ok := s.registroEntrenadores[res.IDWinner]; ok {
		rankingAnterior := ganadorData.Ranking; ganadorData.Ranking += cambioRankingVictoria
		delete(s.inscripciones, ganadorData.ID) // DESINSCRIBIR GANADOR
		log.Printf("LCP: %s (Ganador) ranking: %d->%d. Desinscrito torneo %s.", ganadorData.Nombre, rankingAnterior, ganadorData.Ranking, res.TorneoID)
		datosEv := DatosCambioRanking{EntrenadorID: ganadorData.ID, EntrenadorNombre: ganadorData.Nombre, RankingAnterior: rankingAnterior, RankingNuevo: ganadorData.Ranking, ResultadoCombate: "victoria", OponenteNombre: nombrePerdedor, TorneoID: res.TorneoID, CombateID: res.CombateID}
		s.publicarEventoGeneralASNP("cambio_ranking", datosEv)
	} else { log.Printf("LCP WARN: No se encontró ganador ID '%s' (%s) en registro.", res.IDWinner, res.NombreWinner) }

	if perdedorData, ok := s.registroEntrenadores[idPerdedor]; ok {
		rankingAnterior := perdedorData.Ranking; perdedorData.Ranking += cambioRankingDerrota
		if perdedorData.Ranking < 0 { perdedorData.Ranking = 0 }
		delete(s.inscripciones, perdedorData.ID) // DESINSCRIBIR PERDEDOR
		log.Printf("LCP: %s (Perdedor) ranking: %d->%d. Desinscrito torneo %s.", perdedorData.Nombre, rankingAnterior, perdedorData.Ranking, res.TorneoID)
		datosEv := DatosCambioRanking{EntrenadorID: perdedorData.ID, EntrenadorNombre: perdedorData.Nombre, RankingAnterior: rankingAnterior, RankingNuevo: perdedorData.Ranking, ResultadoCombate: "derrota", OponenteNombre: res.NombreWinner, TorneoID: res.TorneoID, CombateID: res.CombateID}
		s.publicarEventoGeneralASNP("cambio_ranking", datosEv)
	} else { log.Printf("LCP WARN: No se encontró perdedor ID '%s' (%s) en registro.", idPerdedor, nombrePerdedor) }
}

func (s *lcpServer) consumirResultadosCDP() {
	if s.rabbitMQChannelConsumo == nil { log.Printf("LCP ERROR: Canal consumo CDP es nil."); return }
	log.Printf("LCP: Iniciando consumidor resultados CDP (Cola: %s, RK: %s)...", lcpConsumeResultadosQueue, routingKeyResultadosDesdeCDP)
	var msgs <-chan amqp.Delivery; var err error
	msgs, err = s.rabbitMQChannelConsumo.Consume(lcpConsumeResultadosQueue, "lcp_cdp_consumer", false, false, false, false, nil)
	if err != nil { log.Printf("LCP ERROR: Falló registrar consumidor '%s': %v", lcpConsumeResultadosQueue, err); return }
	var wg sync.WaitGroup
	for { select {
		case d, ok := <-msgs:
			if !ok { log.Println("LCP: Canal mensajes CDP cerrado."); wg.Wait(); return }
			log.Printf("LCP: Recibido resultado CDP: %s", string(d.Body)); wg.Add(1)
			go func(delivery amqp.Delivery) {
				defer wg.Done(); var resCDP ResultadoCombateDesdeCDP
				errU := json.Unmarshal(delivery.Body, &resCDP)
				if errU != nil { log.Printf("LCP ERROR: Falló decodificar JSON CDP: %v. Msg: %s", errU, string(delivery.Body)); delivery.Nack(false, false); return }
				s.procesarResultadoDeCombate(resCDP); delivery.Ack(false)
			}(d)
		case <-s.shutdownChan: log.Println("LCP: Consumidor CDP señal apagado."); wg.Wait(); log.Println("LCP: Consumidor CDP detenido."); return
	} }
}

func main() {
	rand.Seed(time.Now().UnixNano()); log.SetFlags(log.Ltime | log.Lshortfile); log.Println("LCP: Iniciando...")
	var err error; var connRabbit *amqp.Connection
	connRabbit, err = amqp.Dial(rabbitMQURL); if err != nil { log.Fatalf("LCP: Falló conectar RabbitMQ: %s", err) }; defer connRabbit.Close(); log.Println("LCP: Conectado RabbitMQ.")
	var servidorLCP *lcpServer; servidorLCP, err = nuevoLCPServer(connRabbit); if err != nil { log.Fatalf("LCP: Falló crear servidor: %v", err) }
	if servidorLCP.rabbitMQChannelPublicacion != nil { defer servidorLCP.rabbitMQChannelPublicacion.Close() }
	if servidorLCP.rabbitMQChannelConsumo != nil { defer servidorLCP.rabbitMQChannelConsumo.Close() }
	if servidorLCP.shutdownChan != nil { defer close(servidorLCP.shutdownChan) }

	osSignals := make(chan os.Signal, 1); signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
	go func() { sig := <-osSignals; log.Printf("LCP: Recibida señal %v, apagando...", sig) }() // Solo loguea, el defer de main cerrará recursos

	var lis net.Listener; lis, err = net.Listen("tcp", lcpPort); if err != nil { log.Fatalf("LCP: Falló escuchar: %v", err) }
	grpcServer := grpc.NewServer(); pb.RegisterLigaPokemonServer(grpcServer, servidorLCP)
	log.Printf("LCP: Servidor gRPC escuchando en %s", lis.Addr());
	go func() { err = grpcServer.Serve(lis); if err != nil { log.Fatalf("LCP: Falló servir gRPC: %v", err) } }()
	
	<-osSignals // Esperar la señal de apagado para terminar main
	log.Println("LCP: Servidor gRPC deteniéndose...")
	grpcServer.GracefulStop() // Intenta un apagado gradual del servidor gRPC
	log.Println("LCP: Servidor gRPC detenido.")
	// Las goroutines de RabbitMQ (consumidor y generador de torneos) deberían detenerse con shutdownChan
}