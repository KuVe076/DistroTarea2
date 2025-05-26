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

	pb "DISTROTAREA2/proto" 
	"github.com/streadway/amqp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Constantes de configuración para el servidor LCP.
// Incluyen puertos, URLs, nombres de exchanges y colas de RabbitMQ, archivos de registro, IDs y parámetros de torneos.
const (
    lcpPort                     = ":50051"                         // Puerto donde escucha el servidor gRPC de la LCP
    rabbitMQURL                 = "amqp://guest:guest@rabbitmq:5672/" // URL de conexión a RabbitMQ
    lcpEventsExchange           = "lcp_events_exchange"            // Exchange para eventos generales de la LCP
    routingKeyLCPEvent          = "lcp.event"                      // Routing key para eventos de la LCP
    lcpRegistroEntrenadoresFile = "entrenadores_pequeno.json"      // Archivo JSON con el registro de entrenadores
    idEntrenadorManual          = "MANUAL001"                      // ID reservado para el entrenador manual
    intervaloGeneracionTorneo   = 25 * time.Second                 // Intervalo entre generación automática de torneos
    maximoTorneosActivos        = 15                               // Máximo de torneos activos permitidos
    torneosIniciales            = 5                                // Número de torneos iniciales al arrancar

    cdpPublicaResultadosExchange = "cdp_validados_exchange"        // Exchange donde la CDP publica resultados validados
    lcpConsumeResultadosQueue    = "lcp_consume_resultados_cdp_queue" // Cola donde la LCP consume resultados desde la CDP
    routingKeyResultadosDesdeCDP = "resultado.validado.lcp"        // Routing key para resultados validados desde la CDP
)

// EntrenadorLCPData representa la estructura de los datos de un entrenador en la LCP.
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type EntrenadorLCPData struct {
    ID         string `json:"id"`         // ID único del entrenador
    Nombre     string `json:"nombre"`     // Nombre del entrenador
    Region     string `json:"region"`     // Región a la que pertenece el entrenador
    Ranking    int    `json:"ranking"`    // Ranking actual del entrenador
    Estado     string `json:"estado"`     // Estado actual (Activo, Suspendido, Expulsado)
    Suspension int    `json:"suspension"` // Número de torneos restantes de suspensión
}

// EventoLCPGeneral representa la estructura de un evento general publicado por la LCP.
// Incluye el tipo de evento, los datos asociados (pueden ser de cualquier tipo) y un timestamp.
type EventoLCPGeneral struct {
    TipoEvento string      `json:"tipo_evento"` // Tipo de evento (ej: "nuevo_torneo_disponible", "inscripcion_exitosa", etc.)
    Datos      interface{} `json:"datos"`       // Datos asociados al evento (estructura variable según el tipo de evento)
    Timestamp  string      `json:"timestamp"`   // Momento en que se generó el evento (formato RFC3339)
}

// DatosInscripcionEvento representa la estructura de los datos de un evento de inscripción en un torneo.
// Incluye información del entrenador, el torneo, la región y un mensaje adicional opcional.
type DatosInscripcionEvento struct {
    EntrenadorID     string `json:"entrenador_id"`           // ID del entrenador que se inscribe
    EntrenadorNombre string `json:"entrenador_nombre"`        // Nombre del entrenador
    TorneoID         string `json:"torneo_id"`                // ID del torneo al que se inscribe
    RegionTorneo     string `json:"region_torneo,omitempty"`  // Región del torneo (opcional)
    MensajeAdicional string `json:"mensaje_adicional,omitempty"` // Mensaje adicional (opcional)
}

// DatosNuevoTorneoEvento representa la estructura de los datos de un evento de creación de un nuevo torneo.
// Incluye el ID del torneo, la región y la fecha de creación.
type DatosNuevoTorneoEvento struct {
    TorneoID      string `json:"torneo_id"`      // ID del torneo creado
    RegionTorneo  string `json:"region_torneo"`  // Región a la que pertenece el torneo
    FechaCreacion string `json:"fecha_creacion"` // Fecha y hora de creación del torneo
}

// DatosCambioRanking representa la estructura de los datos de un evento de cambio de ranking de un entrenador.
// Incluye información del entrenador, ranking anterior y nuevo, resultado del combate, nombre del oponente, torneo y combate.
type DatosCambioRanking struct {
    EntrenadorID     string `json:"entrenador_id"`      // ID del entrenador cuyo ranking cambió
    EntrenadorNombre string `json:"entrenador_nombre"`   // Nombre del entrenador
    RankingAnterior  int    `json:"ranking_anterior"`    // Ranking antes del combate
    RankingNuevo     int    `json:"ranking_nuevo"`       // Ranking después del combate
    ResultadoCombate string `json:"resultado_combate"`   // Resultado del combate ("victoria" o "derrota")
    OponenteNombre   string `json:"oponente_nombre"`     // Nombre del oponente en el combate
    TorneoID         string `json:"torneo_id"`           // ID del torneo donde ocurrió el combate
    CombateID        string `json:"combate_id"`          // ID del combate
}

// ResultadoCombateDesdeCDP representa la estructura de los datos de un resultado de combate recibido desde la CDP (Central de Procesamiento).
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type ResultadoCombateDesdeCDP struct {
    TorneoID          string `json:"torneo_id"`           // ID del torneo al que pertenece el combate
    CombateID         string `json:"combate_id"`          // ID único del combate
    IDEntrenador1     string `json:"id_entrenador_1"`     // ID del primer entrenador participante
    NombreEntrenador1 string `json:"nombre_entrenador_1"` // Nombre del primer entrenador
    IDEntrenador2     string `json:"id_entrenador_2"`     // ID del segundo entrenador participante
    NombreEntrenador2 string `json:"nombre_entrenador_2"` // Nombre del segundo entrenador
    IDWinner          string `json:"id_winner"`           // ID del entrenador ganador
    NombreWinner      string `json:"nombre_winner"`       // Nombre del entrenador ganador
    Fecha             string `json:"fecha"`               // Fecha en la que se realizó el combate
    TipoMensaje       string `json:"tipo_mensaje"`        // Tipo de mensaje (puede ser usado para distinguir eventos)
}

// lcpServer representa el servidor principal de la LCP (Liga Pokémon).
// Contiene toda la información y recursos necesarios para gestionar torneos, inscripciones, entrenadores y comunicación con gimnasios y RabbitMQ.
type lcpServer struct {
    pb.UnimplementedLigaPokemonServer           // Embebe la implementación no utilizada del servidor gRPC generado por protobuf
    mu                            sync.Mutex    // Mutex para proteger el acceso concurrente a los mapas y recursos internos
    torneos                       map[string]*pb.Torneo // Mapa de torneos activos (ID -> Torneo)
    nextTorneoCounter             int           // Contador para generar IDs únicos de torneos
    inscripciones                 map[string]string // Mapa de inscripciones (ID entrenador -> ID torneo)
    registroEntrenadores          map[string]*EntrenadorLCPData // Registro de entrenadores (ID -> datos)
    rabbitMQChannelPublicacion    *amqp.Channel   // Canal de RabbitMQ para publicar eventos generales
    rabbitMQChannelConsumo        *amqp.Channel   // Canal de RabbitMQ para consumir resultados desde la CDP
    entrenadoresEnEsperaPorTorneo map[string][]*EntrenadorLCPData // Entrenadores esperando oponente por torneo (ID torneo -> lista de entrenadores)
    gimnasioClientsByRegion       map[string]pb.GimnasioClient    // Clientes gRPC para gimnasios por región
    gimnasioAddressesByRegion     map[string]string               // Direcciones de los gimnasios por región
    shutdownChan                  chan struct{}                   // Canal para señalizar apagado del servidor
}

// cargarRegistroEntrenadoresLCP lee un archivo JSON y carga los datos de los entrenadores al registro interno de la LCP.
// Devuelve un mapa con los entrenadores cargados o un mapa vacío si no se puede leer el archivo.
func cargarRegistroEntrenadoresLCP(filename string) (map[string]*EntrenadorLCPData, error) {
    data, errReadFile := os.ReadFile(filename)
    if errReadFile != nil {
        log.Printf("LCP WARN: No se pudo leer el archivo de registro '%s': %v", filename, errReadFile)
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

// nuevoLCPServer inicializa y retorna una nueva instancia del servidor principal de la LCP.
// Configura los canales de RabbitMQ para publicación y consumo, declara los exchanges y colas necesarias,
// carga el registro de entrenadores, conecta a los gimnasios, genera torneos iniciales y lanza goroutines
// para la generación periódica de torneos y el consumo de resultados desde la CDP.
func nuevoLCPServer(connRabbit *amqp.Connection) (*lcpServer, error) {
    var chPub *amqp.Channel
    var errPub error
    // Abre canal para publicar eventos generales
    chPub, errPub = connRabbit.Channel()
    if errPub != nil {
        return nil, fmt.Errorf("LCP: falló abrir canal Rabbit para publicación: %w", errPub)
    }
    // Declara el exchange para eventos generales de la LCP
    errPub = chPub.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
    if errPub != nil {
        chPub.Close()
        return nil, fmt.Errorf("LCP: falló declarar exchange de publicación: %w", errPub)
    }

    var chConsume *amqp.Channel
    var errConsume error
    // Abre canal para consumir resultados desde la CDP
    chConsume, errConsume = connRabbit.Channel()
    if errConsume != nil {
        chPub.Close() 
        return nil, fmt.Errorf("LCP: falló al abrir canal Rabbit de consumo: %w", errConsume)
    }
    // Declara el exchange donde la CDP publica resultados validados
    errConsume = chConsume.ExchangeDeclare(cdpPublicaResultadosExchange, "direct", true, false, false, false, nil)
    if errConsume != nil {
        chPub.Close()
        chConsume.Close()
        return nil, fmt.Errorf("LCP: falló al declarar exchange para consumo CDP: %w", errConsume)
    }
    // Declara la cola donde la LCP consumirá resultados desde la CDP
    var q amqp.Queue
    q, errConsume = chConsume.QueueDeclare(lcpConsumeResultadosQueue, true, false, false, false, nil)
    if errConsume != nil {
        chPub.Close()
        chConsume.Close()
        return nil, fmt.Errorf("LCP: falló al declarar cola para consumo CDP: %w", errConsume)
    }
    // Enlaza la cola a la routing key de resultados validados
    errConsume = chConsume.QueueBind(q.Name, routingKeyResultadosDesdeCDP, cdpPublicaResultadosExchange, false, nil)
    if errConsume != nil {
        chPub.Close()
        chConsume.Close()
        return nil, fmt.Errorf("LCP: falló al enlazar cola para consumo CDP: %w", errConsume)
    }

    // Carga el registro de entrenadores desde archivo JSON
    registro, errCarga := cargarRegistroEntrenadoresLCP(lcpRegistroEntrenadoresFile)
    if errCarga != nil {
        log.Printf("LCP ERROR: Falló cargar registro: %v", errCarga)
    }

    // Inicializa la estructura principal del servidor LCP
    s := &lcpServer{
        torneos:                       make(map[string]*pb.Torneo),
        nextTorneoCounter:             1,
        inscripciones:                 make(map[string]string),
        registroEntrenadores:          registro,
        rabbitMQChannelPublicacion:    chPub,
        rabbitMQChannelConsumo:        chConsume,
        entrenadoresEnEsperaPorTorneo: make(map[string][]*EntrenadorLCPData),
        gimnasioClientsByRegion:       make(map[string]pb.GimnasioClient),
        gimnasioAddressesByRegion: map[string]string{
            "Kanto": "gimnasios:50052", "Johto": "gimnasios:50053", "Hoenn": "gimnasios:50054",
            "Sinnoh": "gimnasios:50055", "Teselia": "gimnasios:50056", "Kalos": "gimnasios:50057",
            "Alola": "gimnasios:50058", "Galar": "gimnasios:50059", "Paldea": "gimnasios:50060",
        },
        shutdownChan: make(chan struct{}),
    }
    // Conecta a los gimnasios por región
    s.conectarAGimnasios()
    // Genera los torneos iniciales
    s.generarTorneosIniciales()
    // Lanza goroutine para generación periódica de torneos
    go s.generadorDeTorneosPeriodico()
    // Lanza goroutine para consumir resultados desde la CDP
    go s.consumirResultadosCDP()
    return s, nil
}

// conectarAGimnasios intenta conectar el cliente gRPC de la LCP a todos los gimnasios registrados por región.
// Para cada región, realiza una conexión gRPC al gimnasio correspondiente usando la dirección configurada.
// Si la conexión es exitosa, almacena el cliente gRPC en el mapa gimnasioClientsByRegion.
// Si falla la conexión, muestra una advertencia y continúa con la siguiente región.
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

// generarUnNuevoTorneo crea un nuevo torneo en una región con gimnasio conectado.
// Si no hay gimnasios conectados o se alcanzó el máximo de torneos activos, no crea nada.
// Si notificarSNP es true, publica el evento de nuevo torneo disponible.
// Retorna el puntero al torneo creado o nil si no se pudo crear.
func (s *lcpServer) generarUnNuevoTorneo(notificarSNP bool) *pb.Torneo {
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

// generarTorneosIniciales crea los torneos iniciales al arrancar el servidor LCP.
// Genera hasta 'torneosIniciales' torneos, o hasta alcanzar el máximo permitido.
// Usa un lock para asegurar acceso concurrente seguro.
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

// generadorDeTorneosPeriodico lanza un ciclo que genera torneos automáticamente cada cierto intervalo.
// Utiliza un ticker para esperar el tiempo configurado y, en cada tick, genera un nuevo torneo.
// El ciclo se detiene si recibe una señal de apagado por el canal shutdownChan.
func (s *lcpServer) generadorDeTorneosPeriodico() {
    log.Printf("LCP: Generando torenos de forma periodica (intervalo: %v).", intervaloGeneracionTorneo)
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

// publicarEventoGeneralASNP publica un evento general de la LCP al sistema de notificaciones públicas (SNP) usando RabbitMQ.
// Serializa el evento a JSON y lo publica en el exchange correspondiente.
// Si ocurre un error en la serialización o publicación, lo registra en el log.
func (s *lcpServer) publicarEventoGeneralASNP(tipoEvento string, datos interface{}) {
    evento := EventoLCPGeneral{TipoEvento: tipoEvento, Datos: datos, Timestamp: time.Now().Format(time.RFC3339)}
    var cuerpo []byte
    var errMarshal error
    cuerpo, errMarshal = json.Marshal(evento)
    if errMarshal != nil {
        log.Printf("LCP: Error serializar '%s' SNP: %v", tipoEvento, errMarshal)
        return
    }
    if s.rabbitMQChannelPublicacion == nil {
        log.Printf("LCP ERROR: rabbitMQChannelPublicacion es nil al intentar publicar evento SNP.")
        return
    }

    errPublish := s.rabbitMQChannelPublicacion.Publish(lcpEventsExchange, routingKeyLCPEvent, false, false, amqp.Publishing{ContentType: "application/json", Body: cuerpo})
    if errPublish != nil {
        log.Printf("LCP: Error publicar '%s' SNP: %v", tipoEvento, errPublish)
    } else {
        log.Printf("LCP: Evento '%s' publicado a SNP.", tipoEvento)
    }
}

// ConsultarTorneosDisponibles es un método gRPC que permite consultar la lista de torneos activos disponibles.
// Bloquea el acceso concurrente con un mutex, recopila los torneos y los retorna en la respuesta.
func (s *lcpServer) ConsultarTorneosDisponibles(ctx context.Context, req *pb.ConsultaTorneosReq) (*pb.ListaTorneosResp, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    log.Printf("LCP:'%s' Esta solicitando torneos", req.GetSolicitanteInfo())
    var lista []*pb.Torneo
    for _, t := range s.torneos {
        lista = append(lista, t)
    }
    return &pb.ListaTorneosResp{Torneos: lista}, nil
}

// InscribirEnTorneo es un método gRPC que permite inscribir un entrenador en un torneo.
// Realiza validaciones de estado, región, suspensiones y duplicidad de inscripción.
// Publica eventos de inscripción exitosa o rechazada y gestiona el emparejamiento automático/manual.
func (s *lcpServer) InscribirEnTorneo(ctx context.Context, req *pb.InscripcionTorneoReq) (*pb.ResultadoInscripcionResp, error) {
    s.mu.Lock()
    eID := req.GetEntrenadorId()
    tID := req.GetTorneoId()
    eNombre := req.GetEntrenadorNombre()
    eRegionSolicitud := req.GetEntrenadorRegion()
    log.Printf("LCP: Intento inscripción: ID '%s' (%s, RegiónSol: %s) a TorneoID '%s'", eID, eNombre, eRegionSolicitud, tID)
    entrenadorData, existeEnRegistro := s.registroEntrenadores[eID]
    if !existeEnRegistro {
        // Si el entrenador no existe, lo crea con valores por defecto
        entrenadorData = &EntrenadorLCPData{ID: eID, Nombre: eNombre, Region: eRegionSolicitud, Ranking: 1500, Estado: "Activo", Suspension: 0}
        s.registroEntrenadores[eID] = entrenadorData
        log.Printf("LCP: Entrenador ID '%s' (%s) creado.", eID, eNombre)
    }
    regionEntrenadorVerificada := entrenadorData.Region

    // Rechaza inscripción si el entrenador está expulsado
    if entrenadorData.Estado == "Expulsado" {
        s.mu.Unlock()
        msg := "Estás Expulsado."
        log.Printf("LCP: RECHAZADO (Expulsado) - '%s'. %s", eNombre, msg)
        datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: "Expulsado"}
        s.publicarEventoGeneralASNP("inscripcion_rechazada_expulsado", datosEv)
        return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_ENTRENADOR_EXPULSADO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
    }
    // Si está suspendido, decrementa la suspensión y rechaza si aún le quedan torneos por cumplir
    if entrenadorData.Estado == "Suspendido" {
        entrenadorData.Suspension--
        log.Printf("LCP: Suspensión de '%s' a %d.", eNombre, entrenadorData.Suspension)
        if entrenadorData.Suspension == 0 {
            entrenadorData.Estado = "Activo"
            log.Printf("LCP: '%s' ahora Activo.", eNombre)
        } else {
            s.mu.Unlock()
            msg := fmt.Sprintf("Sigue suspendido por %d torneo(s).", entrenadorData.Suspension)
            log.Printf("LCP: RECHAZADO (Sigue Suspendido) - '%s'. %s", eNombre, msg)
            datosEv := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: tID, MensajeAdicional: fmt.Sprintf("Suspensión restante: %d", entrenadorData.Suspension)}
            s.publicarEventoGeneralASNP("inscripcion_rechazada_suspension", datosEv)
            return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_SUSPENDIDO, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension), NuevoEstadoEntrenador: entrenadorData.Estado}, nil
        }
    }
    torneoInfo := s.torneos[tID]

    // Verifica que la región del torneo coincida con la del entrenador
    if torneoInfo.Region != regionEntrenadorVerificada {
        s.mu.Unlock()
        msg := fmt.Sprintf("Torneo '%s' es para %s, tu región es %s.", tID, torneoInfo.Region, regionEntrenadorVerificada)
        log.Printf("LCP: RECHAZADO (Región Incorrecta) - '%s'. %s", eNombre, msg)
        return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: msg, RazonRechazo: pb.RazonRechazo_RECHAZO_TORNEO_REGION_INCORRECTA, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
    }
    // Verifica si el entrenador ya está inscrito en algún torneo
    torneoActualID, estaInscrito := s.inscripciones[eID]
    if estaInscrito {
        if torneoActualID == tID {
            s.mu.Unlock()
            return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Ya inscrito.", TorneoIdConfirmado: tID, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
        }
        s.mu.Unlock()
        return &pb.ResultadoInscripcionResp{Exito: false, Mensaje: "Inscrito en otro.", RazonRechazo: pb.RazonRechazo_RECHAZO_YA_INSCRITO_EN_OTRO_TORNEO, NuevoEstadoEntrenador: entrenadorData.Estado, NuevaSuspensionEntrenador: int32(entrenadorData.Suspension)}, nil
    }
    // Inscribe al entrenador en el torneo y actualiza su estado
    s.inscripciones[eID] = tID
    entrenadorData.Estado = "Activo"
    entrenadorData.Suspension = 0
    log.Printf("LCP: ÉXITO - '%s' inscrito en Torneo %s.", eNombre, tID)
    datosEvInsc := DatosInscripcionEvento{EntrenadorID: eID, EntrenadorNombre: eNombre, TorneoID: torneoInfo.Id, RegionTorneo: torneoInfo.Region, MensajeAdicional: "Confirmada."}
    s.publicarEventoGeneralASNP("inscripcion_exitosa", datosEvInsc)

    // Si el entrenador es manual, busca emparejarlo con un automático en espera
    esEsteEntrenadorManual := (eID == idEntrenadorManual)
    if esEsteEntrenadorManual {
        log.Printf("LCP: Manual '%s' inscrito. Buscando oponente automático para torneo %s...", eNombre, tID)
        var oponenteAutomatico *EntrenadorLCPData = nil
        var indiceOponente = -1
        for i, esperando := range s.entrenadoresEnEsperaPorTorneo[tID] {
            if esperando.ID != idEntrenadorManual {
                oponenteAutomatico = esperando
                indiceOponente = i
                break
            }
        }
        if oponenteAutomatico != nil {
            // Empareja y asigna combate si hay automático esperando
            s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID][:indiceOponente], s.entrenadoresEnEsperaPorTorneo[tID][indiceOponente+1:]...)
            log.Printf("LCP: Emparejamiento Manual '%s' vs Auto '%s' en torneo %s.", entrenadorData.Nombre, oponenteAutomatico.Nombre, tID)
            s.mu.Unlock()
            s.asignarCombateAGimnasio(torneoInfo, entrenadorData, oponenteAutomatico)
            return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción y emparejamiento con automático asignado.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
        } else {
            // Si no hay automáticos, queda en espera
            s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData)
            log.Printf("LCP: Manual '%s' añadido a espera torneo %s. No hay automáticos.", eNombre, tID)
            s.mu.Unlock()
            return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción. Esperando oponente automático.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
        }
    } else {
        // Si es automático, queda en espera hasta que llegue el manual
        s.entrenadoresEnEsperaPorTorneo[tID] = append(s.entrenadoresEnEsperaPorTorneo[tID], entrenadorData)
        log.Printf("LCP: Automático '%s' añadido a espera torneo %s. Total esperando: %d", eNombre, tID, len(s.entrenadoresEnEsperaPorTorneo[tID]))
        s.mu.Unlock()
        return &pb.ResultadoInscripcionResp{Exito: true, Mensaje: "Inscripción. Automático en espera.", TorneoIdConfirmado: tID, NuevaSuspensionEntrenador: 0, NuevoEstadoEntrenador: "Activo"}, nil
    }
}

// asignarCombateAGimnasio asigna un combate entre dos entrenadores a un gimnasio de la región correspondiente.
// Si no hay cliente gRPC para el gimnasio, ambos entrenadores vuelven a la lista de espera del torneo.
// Genera un ID único para el combate y realiza la llamada gRPC al gimnasio.
// Si el gimnasio acepta el combate, lo informa por log. Si lo rechaza o hay error, los entrenadores vuelven a la espera.
func (s *lcpServer) asignarCombateAGimnasio(torneo *pb.Torneo, e1, e2 *EntrenadorLCPData) {
    regionDelCombate := torneo.Region
    gimnasioCliente, ok := s.gimnasioClientsByRegion[regionDelCombate]
    if !ok {
        log.Printf("LCP ERROR: No cliente gimnasio %s. Combate (%s vs %s) no asignado.", regionDelCombate, e1.Nombre, e2.Nombre)
        s.mu.Lock()
        s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...)
        s.mu.Unlock()
        return
    }
    tN := time.Now().UnixNano()
    rS := rand.Intn(10000)
    cID := fmt.Sprintf("combate-%d-%d", tN, rS)
    log.Printf("LCP: Asignando combate ID %s (T:%s, %s vs %s) a gimnasio %s", cID, torneo.Id, e1.Nombre, e2.Nombre, regionDelCombate)
    reqA := &pb.AsignarCombateRequest{
        CombateId:   cID,
        TorneoId:    torneo.Id,
        Entrenador_1: &pb.EntrenadorInfo{Id: e1.ID, Name: e1.Nombre, Ranking: int32(e1.Ranking)},
        Entrenador_2: &pb.EntrenadorInfo{Id: e2.ID, Name: e2.Nombre, Ranking: int32(e2.Ranking)},
        Region:      regionDelCombate,
    }
    ctxA, cancelA := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancelA()
    var respA *pb.AsignarCombateResponse
    var errA error
    respA, errA = gimnasioCliente.AsignarCombate(ctxA, reqA)
    if errA != nil {
        log.Printf("LCP ERROR: Falló gRPC AsignarCombate (Gym:%s, Cmb:%s): %v", regionDelCombate, cID, errA)
        s.mu.Lock()
        s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...)
        s.mu.Unlock()
        return
    }
    if respA.GetAceptado() {
        log.Printf("LCP: Gimnasio %s aceptó combate %s. Msg: %s", regionDelCombate, cID, respA.GetMensaje())
    } else {
        log.Printf("LCP WARN: Gimnasio %s RECHAZÓ combate %s. Msg: %s", regionDelCombate, cID, respA.GetMensaje())
        s.mu.Lock()
        s.entrenadoresEnEsperaPorTorneo[torneo.Id] = append([]*EntrenadorLCPData{e1, e2}, s.entrenadoresEnEsperaPorTorneo[torneo.Id]...)
        s.mu.Unlock()
    }
}

// procesarResultadoDeCombate procesa el resultado de un combate recibido desde la CDP.
// Actualiza el ranking de los entrenadores, desinscribe a ambos del torneo y publica eventos de cambio de ranking.
func (s *lcpServer) procesarResultadoDeCombate(res ResultadoCombateDesdeCDP) {
    s.mu.Lock()
    defer s.mu.Unlock()
    log.Printf("LCP: Procesando resultado combate ID %s (Torneo: %s). Ganador: %s (%s)", res.CombateID, res.TorneoID, res.NombreWinner, res.IDWinner)
    fmt.Printf("\nLCP: --- Resultado Final Combate %s (Torneo %s) ---\nGanador: %s (ID: %s)\n---------------------------------------------------\n\n", res.CombateID, res.TorneoID, res.NombreWinner, res.IDWinner)
    var idPerdedor, nombrePerdedor string
    if res.IDWinner == res.IDEntrenador1 {
        idPerdedor = res.IDEntrenador2
        nombrePerdedor = res.NombreEntrenador2
    } else if res.IDWinner == res.IDEntrenador2 {
        idPerdedor = res.IDEntrenador1
        nombrePerdedor = res.NombreEntrenador1
    } else {
        log.Printf("LCP ERROR: ID ganador '%s' no coincide con entrenadores (%s, %s) combate %s.", res.IDWinner, res.IDEntrenador1, res.IDEntrenador2, res.CombateID)
        return
    }

    cambioRankingVictoria := 50
    cambioRankingDerrota := -30

    // Actualiza ranking y desinscribe al ganador
    if ganadorData, ok := s.registroEntrenadores[res.IDWinner]; ok {
        rankingAnterior := ganadorData.Ranking
        ganadorData.Ranking += cambioRankingVictoria
        delete(s.inscripciones, ganadorData.ID)
        log.Printf("LCP: %s (Ganador) ranking: %d->%d. Desinscrito torneo %s.", ganadorData.Nombre, rankingAnterior, ganadorData.Ranking, res.TorneoID)
        datosEv := DatosCambioRanking{
            EntrenadorID:     ganadorData.ID,
            EntrenadorNombre: ganadorData.Nombre,
            RankingAnterior:  rankingAnterior,
            RankingNuevo:     ganadorData.Ranking,
            ResultadoCombate: "victoria",
            OponenteNombre:   nombrePerdedor,
            TorneoID:         res.TorneoID,
            CombateID:        res.CombateID,
        }
        s.publicarEventoGeneralASNP("cambio_ranking", datosEv)
    } else {
        log.Printf("LCP WARN: No se encontró ganador ID '%s' (%s) en registro.", res.IDWinner, res.NombreWinner)
    }

    // Actualiza ranking y desinscribe al perdedor
    if perdedorData, ok := s.registroEntrenadores[idPerdedor]; ok {
        rankingAnterior := perdedorData.Ranking
        perdedorData.Ranking += cambioRankingDerrota
        if perdedorData.Ranking < 0 {
            perdedorData.Ranking = 0
        }
        delete(s.inscripciones, perdedorData.ID)
        log.Printf("LCP: %s (Perdedor) ranking: %d->%d. Desinscrito torneo %s.", perdedorData.Nombre, rankingAnterior, perdedorData.Ranking, res.TorneoID)
        datosEv := DatosCambioRanking{
            EntrenadorID:     perdedorData.ID,
            EntrenadorNombre: perdedorData.Nombre,
            RankingAnterior:  rankingAnterior,
            RankingNuevo:     perdedorData.Ranking,
            ResultadoCombate: "derrota",
            OponenteNombre:   res.NombreWinner,
            TorneoID:         res.TorneoID,
            CombateID:        res.CombateID,
        }
        s.publicarEventoGeneralASNP("cambio_ranking", datosEv)
    } else {
        log.Printf("LCP WARN: No se encontró perdedor ID '%s' (%s) en registro.", idPerdedor, nombrePerdedor)
    }
}

// consumirResultadosCDP inicia un consumidor de mensajes RabbitMQ para recibir resultados de combates desde la CDP.
// Por cada mensaje recibido, decodifica el JSON, procesa el resultado y responde con ACK o NACK.
// Usa un WaitGroup para esperar la finalización de las goroutines antes de cerrar.
func (s *lcpServer) consumirResultadosCDP() {
    if s.rabbitMQChannelConsumo == nil {
        log.Printf("LCP ERROR: Canal consumo CDP es nil.")
        return
    }
    log.Printf("LCP: Iniciando consumidor resultados CDP (Cola: %s, RK: %s)...", lcpConsumeResultadosQueue, routingKeyResultadosDesdeCDP)
    var msgs <-chan amqp.Delivery
    var err error
    msgs, err = s.rabbitMQChannelConsumo.Consume(lcpConsumeResultadosQueue, "lcp_cdp_consumer", false, false, false, false, nil)
    if err != nil {
        log.Printf("LCP ERROR: Falló registrar consumidor '%s': %v", lcpConsumeResultadosQueue, err)
        return
    }
    var wg sync.WaitGroup
    for {
        select {
        case d, ok := <-msgs:
            if !ok {
                log.Println("LCP: Canal mensajes CDP cerrado.")
                wg.Wait()
                return
            }
            log.Printf("LCP: Recibido resultado CDP: %s", string(d.Body))
            wg.Add(1)
            go func(delivery amqp.Delivery) {
                defer wg.Done()
                var resCDP ResultadoCombateDesdeCDP
                errU := json.Unmarshal(delivery.Body, &resCDP)
                if errU != nil {
                    log.Printf("LCP ERROR: Falló decodificar JSON CDP: %v. Msg: %s", errU, string(delivery.Body))
                    delivery.Nack(false, false)
                    return
                }
                s.procesarResultadoDeCombate(resCDP)
                delivery.Ack(false)
            }(d)
        case <-s.shutdownChan:
            log.Println("LCP: Consumidor CDP señal apagado.")
            wg.Wait()
            log.Println("LCP: Consumidor CDP detenido.")
            return
        }
    }
}

// main es el punto de entrada del servidor LCP.
// Inicializa el generador de números aleatorios y la configuración del log.
// Establece la conexión con RabbitMQ y crea el servidor principal de la LCP.
// Registra el servidor gRPC y queda a la espera de señales del sistema para apagado controlado.
func main() {
    rand.Seed(time.Now().UnixNano()) // Inicializa la semilla para números aleatorios
    log.SetFlags(log.Ltime | log.Lshortfile) // Configura el formato del log
    log.Println("LCP: Iniciando...")

    var err error
    var connRabbit *amqp.Connection
    connRabbit, err = amqp.Dial(rabbitMQURL) // Conecta a RabbitMQ
    if err != nil {
        log.Fatalf("LCP: Falló conectar RabbitMQ: %s", err)
    }
    defer connRabbit.Close()
    log.Println("LCP: Conectado RabbitMQ.")

    var servidorLCP *lcpServer
    servidorLCP, err = nuevoLCPServer(connRabbit) // Inicializa el servidor LCP
    if err != nil {
        log.Fatalf("LCP: Falló crear servidor: %v", err)
    }
    if servidorLCP.rabbitMQChannelPublicacion != nil {
        defer servidorLCP.rabbitMQChannelPublicacion.Close()
    }
    if servidorLCP.rabbitMQChannelConsumo != nil {
        defer servidorLCP.rabbitMQChannelConsumo.Close()
    }
    if servidorLCP.shutdownChan != nil {
        defer close(servidorLCP.shutdownChan)
    }

    // Maneja señales del sistema para apagado controlado (Ctrl+C, kill)
    osSignals := make(chan os.Signal, 1)
    signal.Notify(osSignals, syscall.SIGINT, syscall.SIGTERM)
    go func() { 
        sig := <-osSignals
        log.Printf("LCP: Recibida señal %v, apagando...", sig)
    }()

    // Inicia el listener TCP para el servidor gRPC
    var lis net.Listener
    lis, err = net.Listen("tcp", lcpPort)
    if err != nil {
        log.Fatalf("LCP: Falló escuchar: %v", err)
    }
    grpcServer := grpc.NewServer()
    pb.RegisterLigaPokemonServer(grpcServer, servidorLCP)
    log.Printf("LCP: Servidor gRPC escuchando en %s", lis.Addr())

    // Lanza el servidor gRPC en una goroutine
    go func() {
        err = grpcServer.Serve(lis)
        if err != nil {
            log.Fatalf("LCP: Falló servir gRPC: %v", err)
        }
    }()

    <-osSignals // Espera señal de apagado
    log.Println("LCP: Servidor gRPC deteniéndose...")
    grpcServer.GracefulStop() // Detiene el servidor gRPC de forma controlada
    log.Println("LCP: Servidor gRPC detenido.")
}
