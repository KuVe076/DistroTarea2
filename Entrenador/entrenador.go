package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "DISTROTAREA2/proto"
	"github.com/streadway/amqp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
    lcpGrpcAddress                 = "lcp:50051" // Dirección del servidor gRPC de la Liga Pokémon Central (LCP)
    rabbitMQURL                    = "amqp://guest:guest@rabbitmq:5672/" // URL de conexión a RabbitMQ
    snpPublishesExchange           = "snp_publishes_notifications_exchange" // Nombre del exchange para notificaciones del SNP
    jsonEntrenadoresFile           = "entrenadores_pequeno.json"      // Archivo JSON con los datos de los entrenadores automáticos
    routingKeyEscuchaNuevosTorneos = "notify.torneo.nuevo_disponible" // Routing key para notificaciones de nuevos torneos
)


// EntrenadorApp representa la aplicación de un entrenador Pokémon.
// Contiene toda la información y recursos necesarios para interactuar con la LCP y RabbitMQ.
type EntrenadorApp struct {
    ID               string              // ID único del entrenador
    Nombre           string              // Nombre del entrenador
    Region           string              // Región a la que pertenece el entrenador
    Ranking          int                 // Ranking actual del entrenador
    Estado           string              // Estado general del entrenador (ej: Activo, Expulsado)
    Suspension       int                 // Número de días de suspensión en la LCP
    TournamentStatus string              // Estado respecto a torneos (ej: InscritoEnLCP, NoInscrito)
    IdTorneoActual   string              // ID del torneo en el que está inscrito actualmente (si aplica)
    EsManual         bool                // Indica si el entrenador es controlado manualmente (por consola)
    mu               sync.Mutex          // Mutex para proteger el acceso concurrente a los datos del entrenador
    lcpCliente       pb.LigaPokemonClient // Cliente gRPC para comunicarse con la Liga Pokémon Central
    rabbitMQChannel  *amqp.Channel       // Canal de RabbitMQ para recibir notificaciones
    notifyQueueName  string              // Nombre de la cola de notificaciones asociada a este entrenador
}

// EntrenadorJSON representa la estructura de los datos de un entrenador
// tal como se encuentran en el archivo JSON de entrada.
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type EntrenadorJSON struct {
    ID         string `json:"id"`         // ID único del entrenador
    Nombre     string `json:"nombre"`     // Nombre del entrenador
    Region     string `json:"region"`     // Región a la que pertenece el entrenador
    Ranking    int    `json:"ranking"`    // Ranking inicial del entrenador
    Estado     string `json:"estado"`     // Estado inicial del entrenador (ej: Activo, Expulsado)
    Suspension int    `json:"suspension"` // Días de suspensión inicial en la LCP
}

// NotificacionParaEntrenador representa la estructura de una notificación enviada a un entrenador.
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type NotificacionParaEntrenador struct {
    TipoNotificacion string `json:"tipo_notificacion"` // Tipo de notificación (ej: CONFIRMACION_INSCRIPCION)
    Mensaje          string `json:"mensaje"`           // Mensaje descriptivo de la notificación
    EntrenadorID     string `json:"entrenador_id,omitempty"` // ID del entrenador al que va dirigida (opcional)
    TorneoID         string `json:"torneo_id,omitempty"`     // ID del torneo relacionado (opcional)
    RegionTorneo     string `json:"region_torneo,omitempty"` // Región del torneo (opcional)
    Timestamp        string `json:"timestamp"`         // Momento en que se generó la notificación
}

// nuevoEntrenadorApp crea e inicializa una nueva instancia de EntrenadorApp.
// Recibe los datos del entrenador (desde JSON o manual), un flag si es manual, la conexión gRPC a la LCP y la conexión a RabbitMQ.
// Configura el canal de RabbitMQ, declara el exchange de notificaciones, crea una cola exclusiva para el entrenador y la enlaza a las routing keys necesarias.
// Devuelve un puntero a EntrenadorApp listo para usar o un error si ocurre algún problema.
func nuevoEntrenadorApp(e EntrenadorJSON, esM bool, lcpC *grpc.ClientConn, rC *amqp.Connection) (*EntrenadorApp, error) {
    app := &EntrenadorApp{
        ID:               e.ID,
        Nombre:           e.Nombre,
        Region:           e.Region,
        Ranking:          e.Ranking,
        Estado:           e.Estado,
        Suspension:       e.Suspension,
        TournamentStatus: "NoInscrito",
        EsManual:         esM,
        lcpCliente:       pb.NewLigaPokemonClient(lcpC),
    }
    var err error
    var q amqp.Queue

    // Crea un canal de RabbitMQ para el entrenador
    app.rabbitMQChannel, err = rC.Channel()
    if err != nil {
        return nil, fmt.Errorf("[%s] Rabbit Chan: %w", app.ID, err)
    }

    // Declara el exchange de notificaciones (asegura que exista)
    err = app.rabbitMQChannel.ExchangeDeclare(snpPublishesExchange, "direct", true, false, false, false, nil)
    if err != nil {
        return nil, fmt.Errorf("[%s] SNP ExDeclare: %w", app.ID, err)
    }

    // Declara una cola exclusiva para el entrenador (nombre único)
    q, err = app.rabbitMQChannel.QueueDeclare(
        fmt.Sprintf("entrenador_%s_q_unified", strings.ReplaceAll(app.ID, " ", "_")),
        false,  // durable
        true,   // auto-delete
        true,   // exclusive
        false,  // no-wait
        nil,    // arguments
    )
    if err != nil {
        return nil, fmt.Errorf("[%s] QDeclare unificada: %w", app.ID, err)
    }
    app.notifyQueueName = q.Name

    // Enlaza la cola a la routing key directa para notificaciones personales
    rkDirecto := fmt.Sprintf("notify.entrenador.%s", app.ID)
    err = app.rabbitMQChannel.QueueBind(app.notifyQueueName, rkDirecto, snpPublishesExchange, false, nil)
    if err != nil {
        return nil, fmt.Errorf("[%s] QBind directo (RK: %s): %w", app.ID, rkDirecto, err)
    }
    log.Printf("[%s] Suscrito a notificaciones directas (Cola: %s, RK: %s)", app.ID, app.notifyQueueName, rkDirecto)

    // Enlaza la cola a la routing key para notificaciones de nuevos torneos
    err = app.rabbitMQChannel.QueueBind(app.notifyQueueName, routingKeyEscuchaNuevosTorneos, snpPublishesExchange, false, nil)
    if err != nil {
        return nil, fmt.Errorf("[%s] QBind nuevos torneos (RK: %s): %w", app.ID, routingKeyEscuchaNuevosTorneos, err)
    }
    log.Printf("[%s] Suscrito también a notificaciones de nuevos torneos (Cola: %s, RK: %s)", app.ID, app.notifyQueueName, routingKeyEscuchaNuevosTorneos)

    return app, nil
}

// escucharNotificaciones escucha y procesa las notificaciones recibidas por el entrenador desde RabbitMQ.
// Decodifica cada mensaje recibido, actualiza el estado del entrenador según el tipo de notificación
// y muestra información relevante por consola. Si el entrenador es manual, imprime un prompt adicional.
func (app *EntrenadorApp) escucharNotificaciones() {
    if app.rabbitMQChannel == nil {
        log.Printf("[%s] RabbitMQ no init.", app.ID)
        return
    }
    var msgs <-chan amqp.Delivery
    var errConsume error
    // Se suscribe a la cola de notificaciones del entrenador
    msgs, errConsume = app.rabbitMQChannel.Consume(app.notifyQueueName, "", true, true, false, false, nil)
    if errConsume != nil {
        log.Printf("[%s] Falló consumidor notif: %v", app.ID, errConsume)
        return
    }

    log.Printf("[%s] Escuchando notificaciones del SNP...", app.ID)
    for d := range msgs {
        // Si es entrenador manual, imprime un salto de línea para mejor visualización
        if app.EsManual {
            fmt.Println()
        }
        // Muestra la notificación recibida (raw)
        log.Printf("[%s] === NOTIFICACIÓN RECIBIDA (RK: %s) ===\n%s\n===============================", app.ID, d.RoutingKey, string(d.Body))
        if app.EsManual {
            fmt.Print("Elige una opción: ")
        }

        // Decodifica el JSON de la notificación
        var notif NotificacionParaEntrenador
        errUnmarshal := json.Unmarshal(d.Body, &notif)
        if errUnmarshal != nil {
            log.Printf("[%s] Error decodificar notif: %v", app.ID, errUnmarshal)
            continue
        }

        log.Printf("[%s] Notificación decodificada: %+v", app.ID, notif)
        app.mu.Lock()
        // Actualiza el estado del entrenador según el tipo de notificación recibida
        switch notif.TipoNotificacion {
        case "CONFIRMACION_INSCRIPCION":
            if notif.EntrenadorID == app.ID {
                app.TournamentStatus = "InscritoEnLCP"
                app.IdTorneoActual = notif.TorneoID
                app.Estado = "Activo"
                app.Suspension = 0
                log.Printf("[%s] Estado actualizado por notificación de inscripción.", app.ID)
            }
        case "RECHAZO_INSCRIPCION_SUSPENSION":
            if notif.EntrenadorID == app.ID {
                log.Printf("[%s] Notificación de rechazo por suspensión recibida.", app.ID)
            }
        case "RECHAZO_INSCRIPCION_EXPULSADO":
            if notif.EntrenadorID == app.ID {
                app.Estado = "Expulsado"
                log.Printf("[%s] Notificación de rechazo por expulsión recibida. Estado actualizado a Expulsado.", app.ID)
            }
        case "NUEVO_TORNEO_DISPONIBLE":
            log.Printf("[%s] ¡Nuevo torneo anunciado! ID: %s, Región: %s. Considera consultarlos.", app.ID, notif.TorneoID, notif.RegionTorneo)
        case "CAMBIO_RANKING":
            if notif.EntrenadorID == app.ID {
                log.Printf("[%s] Notificación de CAMBIO DE RANKING recibida: %s", app.ID, notif.Mensaje)
            }
        }
        app.mu.Unlock()
    }
    log.Printf("[%s] Consumidor de notificaciones terminado.", app.ID)
}

// consultarTorneosYMostrar consulta los torneos disponibles en la LCP usando gRPC
// y muestra la lista de torneos por consola. Retorna el slice de torneos obtenidos.
func (app *EntrenadorApp) consultarTorneosYMostrar(ctx context.Context) []*pb.Torneo {
    log.Printf("[%s] Consultando torneos a LCP...", app.Nombre)
    var resp *pb.ListaTorneosResp
    var err error
    // Realiza la consulta gRPC a la LCP para obtener los torneos disponibles
    resp, err = app.lcpCliente.ConsultarTorneosDisponibles(ctx, &pb.ConsultaTorneosReq{SolicitanteInfo: app.Nombre})
    if err != nil {
        log.Printf("[%s] Error consultar torneos: %v", app.Nombre, err)
        fmt.Printf("Error al consultar torneos.\n")
        return nil
    }
    // Si no hay torneos disponibles, lo informa por consola
    if len(resp.GetTorneos()) == 0 {
        fmt.Printf("[%s] No hay torneos disponibles en este momento.\n", app.Nombre)
        return nil
    }
    // Muestra la lista de torneos disponibles por consola
    fmt.Printf("\n[%s] Torneos Disponibles:\n", app.Nombre)
    for i, t := range resp.GetTorneos() {
        fmt.Printf("  %d. ID: %s, Región: %s\n", i+1, t.Id, t.Region)
    }
    return resp.GetTorneos()
}

// intentarInscripcionTorneo intenta inscribir al entrenador en un torneo específico usando gRPC.
// Actualiza el estado del entrenador según la respuesta de la LCP y muestra mensajes por consola.
func (app *EntrenadorApp) intentarInscripcionTorneo(ctx context.Context, torneoID string) {
    app.mu.Lock()
    estadoActual := app.Estado
    app.mu.Unlock()
    log.Printf("[%s] Intentando inscribirse en torneo %s (Estado LCP conocido: %s)", app.Nombre, torneoID, estadoActual)
    req := &pb.InscripcionTorneoReq{
        TorneoId:        torneoID,
        EntrenadorId:    app.ID,
        EntrenadorNombre: app.Nombre,
        EntrenadorRegion: app.Region,
    }

    var resp *pb.ResultadoInscripcionResp
    var errInscribir error
    // Realiza la solicitud gRPC de inscripción al torneo
    resp, errInscribir = app.lcpCliente.InscribirEnTorneo(ctx, req)
    if errInscribir != nil {
        log.Printf("[%s] Error gRPC al inscribir: %v", app.Nombre, errInscribir)
        fmt.Printf("[%s] Falló la inscripción: %v\n", app.Nombre, errInscribir)
        return
    }

    app.mu.Lock()
    // Actualiza la suspensión y el estado según la respuesta de la LCP
    app.Suspension = int(resp.GetNuevaSuspensionEntrenador())
    app.Estado = resp.GetNuevoEstadoEntrenador()
    if resp.GetExito() {
        // Inscripción exitosa
        app.TournamentStatus = "InscritoEnLCP"
        app.IdTorneoActual = resp.GetTorneoIdConfirmado()
        fmt.Printf("[%s] ¡Inscripción exitosa al torneo %s! %s\n", app.Nombre, resp.GetTorneoIdConfirmado(), resp.GetMensaje())
    } else {
        // Inscripción rechazada
        app.TournamentStatus = "RechazadoPorLCP"
        app.IdTorneoActual = ""
        fmt.Printf("[%s] Inscripción rechazada: %s (Razón: %s). Nueva suspensión LCP: %d, Nuevo estado LCP: %s\n",
            app.Nombre, resp.GetMensaje(), resp.GetRazonRechazo(), app.Suspension, app.Estado)
    }
    app.mu.Unlock()
}

// mostrarEstadoActual imprime por consola el estado actual del entrenador.
// Muestra información como nombre, ID, región, ranking, estado general, suspensión, estado en torneo y, si aplica, el ID del torneo actual.
func (app *EntrenadorApp) mostrarEstadoActual() {
    app.mu.Lock()
    defer app.mu.Unlock()
    fmt.Printf("\n--- Estado de %s (ID:%s) ---\nRegión: %s\nRanking: %d\nEstado General LCP: %s\nSuspensión LCP: %d\nEstado Torneo: %s", app.Nombre, app.ID, app.Region, app.Ranking, app.Estado, app.Suspension, app.TournamentStatus)
    if app.IdTorneoActual != "" {
        fmt.Printf(" (En Torneo ID: %s)", app.IdTorneoActual)
    }
    fmt.Println("\n-------------------------------")
}

// cargarEntrenadoresJSON lee un archivo JSON y retorna un slice con los datos de los entrenadores.
// Si ocurre un error al leer o decodificar el archivo, lo retorna.
func cargarEntrenadoresJSON(f string) ([]EntrenadorJSON, error) {
    var d []byte
    var errRF error
    d, errRF = os.ReadFile(f)
    if errRF != nil {
        return nil, fmt.Errorf("leer %s: %w", f, errRF)
    }
    var e []EntrenadorJSON
    errU := json.Unmarshal(d, &e)
    if errU != nil {
        return nil, fmt.Errorf("JSON %s: %w", f, errU)
    }
    return e, nil
}

// obtenerDatosEntrenadorManual solicita por consola los datos básicos de un entrenador manual.
// Devuelve una estructura EntrenadorJSON con los datos ingresados y valores por defecto.
func obtenerDatosEntrenadorManual() EntrenadorJSON {
    r := bufio.NewReader(os.Stdin)
    fmt.Println("\n--- Configuración Entrenador Consola ---")
    var id, n, reg string
    // Solicita el ID del entrenador hasta que se ingrese uno válido
    for {
        fmt.Print("ID Entrenador: ")
        id, _ = r.ReadString('\n')
        id = strings.TrimSpace(id)
        if id != "" {
            break
        }
    }
    // Solicita el nombre del entrenador hasta que se ingrese uno válido
    for {
        fmt.Print("Nombre: ")
        n, _ = r.ReadString('\n')
        n = strings.TrimSpace(n)
        if n != "" {
            break
        }
    }
    // Solicita la región del entrenador hasta que se ingrese una válida
    for {
        fmt.Print("Región: ")
        reg, _ = r.ReadString('\n')
        reg = strings.TrimSpace(reg)
        if reg != "" {
            break
        }
    }
    // Retorna la estructura con los datos ingresados y valores por defecto para ranking, estado y suspensión
    return EntrenadorJSON{ID: "MANUAL001", Nombre: n, Region: reg, Ranking: 1500, Estado: "Activo", Suspension: 0}
}


// simularEntrenadorAutomatico simula el comportamiento de un entrenador automático.
// Intenta inscribirse en torneos disponibles de forma periódica y aleatoria, solo si no está inscrito o expulsado.
func simularEntrenadorAutomatico(ctx context.Context, app *EntrenadorApp) {
    log.Printf("[%s AUTO] Iniciando simulación de comportamiento.", app.Nombre)
    time.Sleep(time.Duration(rand.Intn(5000)+1000) * time.Millisecond)

    for {
        select {
        case <-ctx.Done():
            log.Printf("[%s AUTO] Contexto cancelado, terminando simulación.", app.Nombre)
            return
        default:

            // Verifica si el entrenador puede intentar inscribirse (no inscrito ni expulsado)
            app.mu.Lock()
            puedeIntentarInscripcion := app.TournamentStatus != "InscritoEnLCP" && app.Estado != "Expulsado"
            app.mu.Unlock()

            if puedeIntentarInscripcion {
                log.Printf("[%s AUTO] Intentando inscribirse en un torneo...", app.Nombre)

                // Crea un contexto con timeout para la operación de inscripción
                opCtx, opCancel := context.WithTimeout(ctx, 30*time.Second)

                // Consulta los torneos disponibles
                listaTorneos := app.consultarTorneosYMostrar(opCtx) 
                if len(listaTorneos) > 0 {
                    // Filtra torneos de la misma región
                    var candidatos []*pb.Torneo
                    for _, t := range listaTorneos {
                        if t.Region == app.Region {
                            candidatos = append(candidatos, t)
                        }
                    }

                    // Elige un torneo aleatorio de la región o de la lista general
                    var torneoElegido *pb.Torneo
                    if len(candidatos) > 0 {
                        torneoElegido = candidatos[rand.Intn(len(candidatos))]
                    } else { 
                        torneoElegido = listaTorneos[rand.Intn(len(listaTorneos))]
                    }
                    log.Printf("[%s AUTO] Eligió torneo %s. Intentando inscripción.", app.Nombre, torneoElegido.Id)
                    app.intentarInscripcionTorneo(opCtx, torneoElegido.Id)
                } else {
                    log.Printf("[%s AUTO] No hay torneos disponibles para intentar inscripción.", app.Nombre)
                }
                opCancel()
            }

            // Espera un tiempo aleatorio antes de volver a intentar
            tiempoEspera := time.Duration(rand.Intn(70)+15) * time.Second 
            time.Sleep(tiempoEspera)
        }
    }
}

// main es el punto de entrada de la aplicación Entrenador.
// Inicializa conexiones, crea entrenadores (manual y automáticos), lanza sus goroutines y gestiona el menú interactivo.
func main() {
    rand.Seed(time.Now().UnixNano())
    log.SetFlags(log.Ltime | log.Lshortfile)
    var err error
    var connLCP *grpc.ClientConn
    var connRabbit *amqp.Connection

    // Conexión gRPC a la LCP
    connLCP, err = grpc.Dial(lcpGrpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
    if err != nil {
        log.Fatalf("CLIENTE: No conectar LCP: %v", err)
    }
    defer connLCP.Close()
    log.Println("CLIENTE: Conectado LCP.")

    // Conexión a RabbitMQ
    connRabbit, err = amqp.Dial(rabbitMQURL)
    if err != nil {
        log.Fatalf("CLIENTE: No conectar RabbitMQ: %s", err)
    }
    defer connRabbit.Close()
    log.Println("CLIENTE: Conectado RabbitMQ.")

    // Solicita datos del entrenador manual y lo inicializa
    manualData := obtenerDatosEntrenadorManual()
    entrenadorManualApp, err := nuevoEntrenadorApp(manualData, true, connLCP, connRabbit)
    if err != nil {
        log.Fatalf("Error creando manual: %v", err)
    }
    go entrenadorManualApp.escucharNotificaciones()

    // Carga entrenadores automáticos desde archivo JSON y los inicializa
    entrenadoresJSON, err := cargarEntrenadoresJSON(jsonEntrenadoresFile)
    if err != nil {
        log.Printf("WARN: No cargar JSON %s: %v", jsonEntrenadoresFile, err)
    }
    for _, eJSON := range entrenadoresJSON {
        if eJSON.ID == entrenadorManualApp.ID {
            continue
        }
        app, errAppAuto := nuevoEntrenadorApp(eJSON, false, connLCP, connRabbit)
        if errAppAuto != nil {
            log.Printf("Error creando auto %s: %v", eJSON.ID, errAppAuto)
            continue
        }
        go app.escucharNotificaciones()
        
        autoCtx, autoCancel := context.WithCancel(context.Background())
        defer autoCancel() 
        go simularEntrenadorAutomatico(autoCtx, app)
    }
    log.Printf("Entrenador manual y %d automáticos inicializados", len(entrenadoresJSON))

    // Menú interactivo para el entrenador manual
    reader := bufio.NewReader(os.Stdin)
    for {
        fmt.Printf("\n--- Menú Entrenador [%s] ---\n", entrenadorManualApp.Nombre)
        fmt.Println("1. Consultar Torneos Disponibles")
        fmt.Println("2. Inscribirse en Torneo")
        fmt.Println("3. Ver mi Estado")
        fmt.Println("4. Salir")
        fmt.Print("Elige una opción: ")
        input, _ := reader.ReadString('\n')
        choiceStr := strings.TrimSpace(input)
        choice, errConv := strconv.Atoi(choiceStr)
        if errConv != nil {
            fmt.Println("Opción inválida.")
            continue
        }
        ctxMenu, cancelMenu := context.WithTimeout(context.Background(), 1*time.Minute)
        switch choice {
        case 1:
            // Consultar torneos disponibles
            entrenadorManualApp.consultarTorneosYMostrar(ctxMenu)
        case 2:
            // Inscribirse en torneo seleccionado
            listaParaElegir := entrenadorManualApp.consultarTorneosYMostrar(ctxMenu)
            if len(listaParaElegir) > 0 {
                fmt.Print("Ingresa el NÚMERO del torneo para inscribirte (0 para cancelar): ")
                torneoNumStr, _ := reader.ReadString('\n')
                torneoNum, errAtoi := strconv.Atoi(strings.TrimSpace(torneoNumStr))
                if errAtoi == nil && torneoNum > 0 && torneoNum <= len(listaParaElegir) {
                    entrenadorManualApp.intentarInscripcionTorneo(ctxMenu, listaParaElegir[torneoNum-1].Id)
                } else if torneoNum == 0 {
                    fmt.Println("Inscripción cancelada.")
                } else {
                    fmt.Println("Selección de torneo inválida.")
                }
            }
        case 3:
            // Mostrar estado actual del entrenador
            entrenadorManualApp.mostrarEstadoActual()
        case 4:
            // Salir del programa
            fmt.Println("Saliendo...")
            cancelMenu()
            return 
        default:
            fmt.Println("Opción no válida.")
        }
        cancelMenu()
    }
}
