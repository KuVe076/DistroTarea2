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
	lcpGrpcAddress                 = "lcp:50051"
	rabbitMQURL                    = "amqp://guest:guest@rabbitmq:5672/"
	snpPublishesExchange           = "snp_publishes_notifications_exchange"
	jsonEntrenadoresFile           = "entrenadores_pequeno.json" // Relativo al WORKDIR del contenedor
	routingKeyEscuchaNuevosTorneos = "notify.torneo.nuevo_disponible" // Debe coincidir con SNP
)

// --- Structs (EntrenadorApp, EntrenadorJSON, NotificacionParaEntrenador como antes) ---
type EntrenadorApp struct {ID string; Nombre string; Region string; Ranking int; Estado string; Suspension int; TournamentStatus string; IdTorneoActual string; EsManual bool; mu sync.Mutex; lcpCliente pb.LigaPokemonClient; rabbitMQChannel *amqp.Channel; notifyQueueName string}
type EntrenadorJSON struct {ID string `json:"id"`; Nombre string `json:"nombre"`; Region string `json:"region"`; Ranking int `json:"ranking"`; Estado string `json:"estado"`; Suspension int `json:"suspension"`}
type NotificacionParaEntrenador struct {TipoNotificacion string `json:"tipo_notificacion"`; Mensaje string `json:"mensaje"`; EntrenadorID string `json:"entrenador_id,omitempty"`; TorneoID string `json:"torneo_id,omitempty"`; RegionTorneo string `json:"region_torneo,omitempty"`; Timestamp string `json:"timestamp"`}


func nuevoEntrenadorApp(e EntrenadorJSON, esM bool, lcpC *grpc.ClientConn, rC *amqp.Connection) (*EntrenadorApp, error) {
	app := &EntrenadorApp{ID: e.ID, Nombre: e.Nombre, Region: e.Region, Ranking: e.Ranking, Estado: e.Estado, Suspension: e.Suspension, TournamentStatus: "NoInscrito", EsManual: esM, lcpCliente: pb.NewLigaPokemonClient(lcpC)}
	var err error; var q amqp.Queue
	app.rabbitMQChannel, err = rC.Channel(); if err != nil { return nil, fmt.Errorf("[%s] Rabbit Chan: %w", app.ID, err) }
	
	err = app.rabbitMQChannel.ExchangeDeclare(snpPublishesExchange, "direct", true, false, false, false, nil) // Asegurar que el exchange exista
	if err != nil { return nil, fmt.Errorf("[%s] SNP ExDeclare: %w", app.ID, err) }

	// Cola para notificaciones directas y generales
	q, err = app.rabbitMQChannel.QueueDeclare(
		fmt.Sprintf("entrenador_%s_q_unified", strings.ReplaceAll(app.ID, " ", "_")), // Nombre único
		false, true, true, false, nil, // non-durable, auto-delete, exclusive
	)
	if err != nil { return nil, fmt.Errorf("[%s] QDeclare unificada: %w", app.ID, err) }
	app.notifyQueueName = q.Name

	// Binding para notificaciones directas
	rkDirecto := fmt.Sprintf("notify.entrenador.%s", app.ID)
	err = app.rabbitMQChannel.QueueBind(app.notifyQueueName, rkDirecto, snpPublishesExchange, false, nil)
	if err != nil { return nil, fmt.Errorf("[%s] QBind directo (RK: %s): %w", app.ID, rkDirecto, err) }
	log.Printf("[%s] Suscrito a notificaciones directas (Cola: %s, RK: %s)", app.ID, app.notifyQueueName, rkDirecto)

	// Binding ADICIONAL para notificaciones generales de nuevos torneos A LA MISMA COLA
	err = app.rabbitMQChannel.QueueBind(app.notifyQueueName, routingKeyEscuchaNuevosTorneos, snpPublishesExchange, false, nil)
	if err != nil { return nil, fmt.Errorf("[%s] QBind nuevos torneos (RK: %s): %w", app.ID, routingKeyEscuchaNuevosTorneos, err) }
	log.Printf("[%s] Suscrito también a notificaciones de nuevos torneos (Cola: %s, RK: %s)", app.ID, app.notifyQueueName, routingKeyEscuchaNuevosTorneos)

	return app, nil
}

func (app *EntrenadorApp) escucharNotificaciones() {
	if app.rabbitMQChannel == nil { log.Printf("[%s] RabbitMQ no init.", app.ID); return }
	var msgs <-chan amqp.Delivery; var errConsume error
	msgs, errConsume = app.rabbitMQChannel.Consume(app.notifyQueueName, "", true, true, false, false, nil)
	if errConsume != nil { log.Printf("[%s] Falló consumidor notif: %v", app.ID, errConsume); return }

	log.Printf("[%s] Escuchando notificaciones del SNP...", app.ID)
	for d := range msgs {
		// Imprimir antes para el manual, para que el prompt no se sobreescriba
		if app.EsManual { fmt.Println() } // Salto de línea para el prompt
		log.Printf("[%s] === NOTIFICACIÓN RECIBIDA (RK: %s) ===\n%s\n===============================", app.ID, d.RoutingKey, string(d.Body))
		if app.EsManual { fmt.Print("Elige una opción: ") } // Volver a imprimir el prompt

		var notif NotificacionParaEntrenador; var errUnmarshal error
		errUnmarshal = json.Unmarshal(d.Body, &notif)
		if errUnmarshal != nil { log.Printf("[%s] Error decodificar notif: %v", app.ID, errUnmarshal); continue }
		
		log.Printf("[%s] Notificación decodificada: %+v", app.ID, notif)
		app.mu.Lock()
		switch notif.TipoNotificacion {
		case "CONFIRMACION_INSCRIPCION":
			if notif.EntrenadorID == app.ID { // Asegurarse que sea para este entrenador
				app.TournamentStatus = "InscritoEnLCP"; app.IdTorneoActual = notif.TorneoID
				app.Estado = "Activo"; app.Suspension = 0
				log.Printf("[%s] Estado actualizado por notificación de inscripción.", app.ID)
			}
		case "RECHAZO_INSCRIPCION_SUSPENSION":
			if notif.EntrenadorID == app.ID {
				log.Printf("[%s] Notificación de rechazo por suspensión recibida.", app.ID)
			}
		case "RECHAZO_INSCRIPCION_EXPULSADO":
			if notif.EntrenadorID == app.ID {
				app.Estado = "Expulsado" // SNP podría confirmar el estado
				log.Printf("[%s] Notificación de rechazo por expulsión recibida. Estado actualizado a Expulsado.", app.ID)
			}
		case "NUEVO_TORNEO_DISPONIBLE": // <<<--- NUEVO MANEJO
			log.Printf("[%s] ¡Nuevo torneo anunciado! ID: %s, Región: %s. Considera consultarlos.", app.ID, notif.TorneoID, notif.RegionTorneo)
			// El mensaje completo ya se imprimió. El entrenador podría actuar sobre esto si es automático
			// o el manual lo verá y podrá decidir consultar torneos.
		}
		app.mu.Unlock()
	}
	log.Printf("[%s] Consumidor de notificaciones terminado.", app.ID)
}

func (app *EntrenadorApp) consultarTorneosYMostrar(ctx context.Context) []*pb.Torneo {
	log.Printf("[%s] Consultando torneos a LCP...", app.Nombre)
	var resp *pb.ListaTorneosResp
	var err error
	resp, err = app.lcpCliente.ConsultarTorneosDisponibles(ctx, &pb.ConsultaTorneosReq{SolicitanteInfo: app.Nombre})
	if err != nil {
		log.Printf("[%s] Error consultar torneos: %v", app.Nombre, err)
		fmt.Printf("Error al consultar torneos.\n")
		return nil
	}
	if len(resp.GetTorneos()) == 0 {
		fmt.Printf("[%s] No hay torneos disponibles en este momento.\n", app.Nombre)
		return nil
	}
	fmt.Printf("\n[%s] Torneos Disponibles:\n", app.Nombre)
	for i, t := range resp.GetTorneos() {
		fmt.Printf("  %d. ID: %s, Región: %s\n", i+1, t.Id, t.Region)
	}
	return resp.GetTorneos()
}

func (app *EntrenadorApp) intentarInscripcionTorneo(ctx context.Context, torneoID string) {
	app.mu.Lock()
	estadoActual := app.Estado
	app.mu.Unlock() // No necesitamos enviar suspensión, LCP la tiene
	log.Printf("[%s] Intentando inscribirse en torneo %s (Estado LCP conocido: %s)", app.Nombre, torneoID, estadoActual)
	req := &pb.InscripcionTorneoReq{TorneoId: torneoID, EntrenadorId: app.ID, EntrenadorNombre: app.Nombre, EntrenadorRegion: app.Region}

	var resp *pb.ResultadoInscripcionResp
	var errInscribir error
	resp, errInscribir = app.lcpCliente.InscribirEnTorneo(ctx, req)
	if errInscribir != nil {
		log.Printf("[%s] Error gRPC al inscribir: %v", app.Nombre, errInscribir)
		fmt.Printf("[%s] Falló la inscripción: %v\n", app.Nombre, errInscribir)
		return
	}

	app.mu.Lock()
	app.Suspension = int(resp.GetNuevaSuspensionEntrenador()) // Actualizar con lo que dice LCP
	app.Estado = resp.GetNuevoEstadoEntrenador()              // Actualizar con lo que dice LCP
	if resp.GetExito() {
		app.TournamentStatus = "InscritoEnLCP"
		app.IdTorneoActual = resp.GetTorneoIdConfirmado()
		fmt.Printf("[%s] ¡Inscripción exitosa al torneo %s! %s\n", app.Nombre, resp.GetTorneoIdConfirmado(), resp.GetMensaje())
	} else {
		app.TournamentStatus = "RechazadoPorLCP"
		app.IdTorneoActual = ""
		fmt.Printf("[%s] Inscripción rechazada: %s (Razón: %s). Nueva suspensión LCP: %d, Nuevo estado LCP: %s\n",
			app.Nombre, resp.GetMensaje(), resp.GetRazonRechazo(), app.Suspension, app.Estado)
	}
	app.mu.Unlock()
}
func (app *EntrenadorApp) mostrarEstadoActual() {
	app.mu.Lock()
	defer app.mu.Unlock()
	fmt.Printf("\n--- Estado de %s (ID:%s) ---\nRegión: %s\nRanking: %d\nEstado General LCP: %s\nSuspensión LCP: %d\nEstado Torneo: %s", app.Nombre, app.ID, app.Region, app.Ranking, app.Estado, app.Suspension, app.TournamentStatus)
	if app.IdTorneoActual != "" {
		fmt.Printf(" (En Torneo ID: %s)", app.IdTorneoActual)
	}
	fmt.Println("\n-------------------------------")
}

func cargarEntrenadoresJSON(f string) ([]EntrenadorJSON, error) {
	var d []byte
	var errRF error
	d, errRF = os.ReadFile(f)
	if errRF != nil {
		return nil, fmt.Errorf("leer %s: %w", f, errRF)
	}
	var e []EntrenadorJSON
	var errU error
	errU = json.Unmarshal(d, &e)
	if errU != nil {
		return nil, fmt.Errorf("JSON %s: %w", f, errU)
	}
	return e, nil
}
func obtenerDatosEntrenadorManual() EntrenadorJSON {
	r := bufio.NewReader(os.Stdin)
	fmt.Println("\n--- Configuración Entrenador Consola ---")
	var id, n, reg string	
	for { 
		fmt.Print("ID Entrenador: "); 
		id, _ = r.ReadString('\n'); 
		id = strings.TrimSpace(id); 
		if id != "" { 
			break 
			} 
	};
	for {
		fmt.Print("Nombre: ")
		n, _ = r.ReadString('\n')
		n = strings.TrimSpace(n)
		if n != "" {
			break
		}
	}
	for {
		fmt.Print("Región: ")
		reg, _ = r.ReadString('\n')
		reg = strings.TrimSpace(reg)
		if reg != "" {
			break
		}
	}
	return EntrenadorJSON{ID: "MANUAL001", Nombre: n, Region: reg, Ranking: 1500, Estado: "Activo", Suspension: 0}
}

// Simula el comportamiento de un entrenador automático
func simularEntrenadorAutomatico(ctx context.Context, app *EntrenadorApp) {
	log.Printf("[%s AUTO] Iniciando simulación de comportamiento.", app.Nombre)
	// Pequeño delay inicial aleatorio para desfasar los automáticos
	time.Sleep(time.Duration(rand.Intn(5000)+1000) * time.Millisecond)

	for { // Bucle de "vida" del entrenador automático
		select {
		case <-ctx.Done(): // Si el contexto principal del programa se cancela
			log.Printf("[%s AUTO] Contexto cancelado, terminando simulación.", app.Nombre)
			return
		default:
			// Lógica de decisión del entrenador automático
			app.mu.Lock()
			puedeIntentarInscripcion := app.TournamentStatus != "InscritoEnLCP" && app.Estado != "Expulsado"
			app.mu.Unlock()

			if puedeIntentarInscripcion {
				log.Printf("[%s AUTO] Intentando inscribirse en un torneo...", app.Nombre)
				// Crear un contexto para esta operación específica
				opCtx, opCancel := context.WithTimeout(ctx, 30*time.Second)

				listaTorneos := app.consultarTorneosYMostrar(opCtx) // Consultar y obtener la lista
				if len(listaTorneos) > 0 {
					var candidatos []*pb.Torneo
					for _, t := range listaTorneos {
						if t.Region == app.Region {
							candidatos = append(candidatos, t)
						}
					}

					var torneoElegido *pb.Torneo
					if len(candidatos) > 0 {
						torneoElegido = candidatos[rand.Intn(len(candidatos))]
					} else { // Si no hay en su región, elige cualquiera
						torneoElegido = listaTorneos[rand.Intn(len(listaTorneos))]
					}
					log.Printf("[%s AUTO] Eligió torneo %s. Intentando inscripción.", app.Nombre, torneoElegido.Id)
					app.intentarInscripcionTorneo(opCtx, torneoElegido.Id)
				} else {
					log.Printf("[%s AUTO] No hay torneos disponibles para intentar inscripción.", app.Nombre)
				}
				opCancel()
			}
			// Esperar un tiempo aleatorio antes del próximo intento/acción
			// Esto simula que el entrenador hace otras cosas o que los eventos de torneo ocurren espaciados.
			// La "Regla de Suspensión" implica que el intento de inscripción es lo que decrementa.
			tiempoEspera := time.Duration(rand.Intn(70)+15) * time.Second // Entre 15 y 40 segundos
			// log.Printf("[%s AUTO] Esperando %v para la próxima acción.", app.Nombre, tiempoEspera)
			time.Sleep(tiempoEspera)
		}
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	log.SetFlags(log.Ltime | log.Lshortfile)
	var err error
	var connLCP *grpc.ClientConn
	var connRabbit *amqp.Connection
	connLCP, err = grpc.Dial(lcpGrpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		log.Fatalf("CLIENTE: No conectar LCP: %v", err)
	}
	defer connLCP.Close()
	log.Println("CLIENTE: Conectado LCP.")
	connRabbit, err = amqp.Dial(rabbitMQURL)
	if err != nil {
		log.Fatalf("CLIENTE: No conectar RabbitMQ: %s", err)
	}
	defer connRabbit.Close()
	log.Println("CLIENTE: Conectado RabbitMQ.")

	manualData := obtenerDatosEntrenadorManual()
	entrenadorManualApp, err := nuevoEntrenadorApp(manualData, true, connLCP, connRabbit)
	if err != nil {
		log.Fatalf("Error creando manual: %v", err)
	}
	go entrenadorManualApp.escucharNotificaciones()

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
		// Crear un contexto para la goroutine del entrenador automático que pueda ser cancelado
		autoCtx, autoCancel := context.WithCancel(context.Background())
		defer autoCancel() // Asegurar que se cancele al salir de main (o manejarlo más granularmente)
		go simularEntrenadorAutomatico(autoCtx, app)
	}
	log.Printf("Entrenador manual y %d automáticos inicializados (si se cargó JSON).", len(entrenadoresJSON))

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
			entrenadorManualApp.consultarTorneosYMostrar(ctxMenu)
		case 2:
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
			entrenadorManualApp.mostrarEstadoActual()
		case 4:
			fmt.Println("Saliendo...")
			cancelMenu()
			return // Salir del programa
		default:
			fmt.Println("Opción no válida.")
		}
		cancelMenu()
	}
}
