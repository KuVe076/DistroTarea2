package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/streadway/amqp"
)

const (
	rabbitMQURL            = "amqp://guest:guest@rabbitmq:5672/"
	lcpEventsExchange      = "lcp_events_exchange"           // Exchange del que SNP consume
	snpConsumesQueueName   = "snp_consumes_lcp_events_queue" // Cola de SNP para eventos de LCP
	routingKeyLCPEvent     = "lcp.event"                     // SNP escucha este RK genérico de LCP
	snpPublishesExchange   = "snp_publishes_notifications_exchange" // Exchange donde SNP publica para entrenadores
	// Routing key para que SNP publique notificaciones GENERALES de nuevos torneos
	routingKeySNPNuevoTorneo = "notify.torneo.nuevo_disponible"
)

// EventoLCPGeneral (debe coincidir con la definición en lcp_server.go)
type EventoLCPGeneral struct {
	TipoEvento string      `json:"tipo_evento"`
	Datos      interface{} `json:"datos"` // El payload real del evento
	Timestamp  string      `json:"timestamp"`
}

// DatosInscripcionEvento (payload para eventos de inscripción)
type DatosInscripcionEvento struct {
	EntrenadorID     string `json:"entrenador_id"`
	EntrenadorNombre string `json:"entrenador_nombre"`
	TorneoID         string `json:"torneo_id"`
	RegionTorneo     string `json:"region_torneo,omitempty"`
	MensajeAdicional string `json:"mensaje_adicional,omitempty"`
}

// DatosNuevoTorneoEvento (payload para evento de nuevo torneo)
type DatosNuevoTorneoEvento struct {
	TorneoID      string `json:"torneo_id"`
	RegionTorneo  string `json:"region_torneo"`
	FechaCreacion string `json:"fecha_creacion"`
}

// NotificacionParaEntrenador es lo que SNP envía al entrenador
type NotificacionParaEntrenador struct {
	TipoNotificacion string `json:"tipo_notificacion"`
	Mensaje          string `json:"mensaje"`
	EntrenadorID     string `json:"entrenador_id,omitempty"` // Opcional para notificaciones generales
	TorneoID         string `json:"torneo_id,omitempty"`
	RegionTorneo     string `json:"region_torneo,omitempty"`
	Timestamp        string `json:"timestamp"`
}

func failOnError(err error, msg string) {
	if err != nil {
		log.Fatalf("SNP: %s: %s", msg, err)
	}
}

func publicarNotificacion(ch *amqp.Channel, notificacion NotificacionParaEntrenador, routingKey string) {
	cuerpo, errMarshal := json.Marshal(notificacion)
	if errMarshal != nil { log.Printf("SNP: Error serializar notificación: %v", errMarshal); return }
	
	errPublish := ch.Publish(
		snpPublishesExchange, // Publicar al exchange de notificaciones SNP
		routingKey,           // Usar el routing key determinado
		false, false,
		amqp.Publishing{ContentType: "application/json", Body: cuerpo})
	if errPublish != nil { log.Printf("SNP: Error publicar notif (RK:%s): %v", routingKey, errPublish) } else {
		log.Printf("SNP: Notificación '%s' publicada (RK: %s)", notificacion.TipoNotificacion, routingKey)
	}
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile)
	log.Println("SNP: Iniciando...")
	var conn *amqp.Connection
	var err error
	conn, err = amqp.Dial(rabbitMQURL)
	failOnError(err, "Falló conectar RabbitMQ")
	defer conn.Close()
	log.Println("SNP: Conectado a RabbitMQ.")

	var ch *amqp.Channel
	ch, err = conn.Channel()
	failOnError(err, "Falló abrir canal")
	defer ch.Close()

	// Exchange para consumir eventos de LCP (asegurarse que exista)
	err = ch.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
	failOnError(err, "Falló declarar exchange LCP")

	var qConsume amqp.Queue
	qConsume, err = ch.QueueDeclare(snpConsumesQueueName, true, false, false, false, nil) // Durable queue
	failOnError(err, fmt.Sprintf("Falló declarar cola '%s'", snpConsumesQueueName))

	// SNP se bindea al routing key genérico de eventos de LCP
	err = ch.QueueBind(qConsume.Name, routingKeyLCPEvent, lcpEventsExchange, false, nil)
	failOnError(err, fmt.Sprintf("Falló bindear cola '%s' a exchange '%s' con RK '%s'", qConsume.Name, lcpEventsExchange, routingKeyLCPEvent))
	log.Printf("SNP: Cola '%s' bindeada para eventos de LCP (RK: %s).", qConsume.Name, routingKeyLCPEvent)

	// Exchange para publicar notificaciones a entrenadores (asegurarse que exista)
	err = ch.ExchangeDeclare(snpPublishesExchange, "direct", true, false, false, false, nil)
	failOnError(err, "Falló declarar exchange de notificaciones SNP")
	log.Printf("SNP: Exchange de notificaciones '%s' declarado.", snpPublishesExchange)

	var msgsFromLCP <-chan amqp.Delivery
	msgsFromLCP, err = ch.Consume(qConsume.Name, "", true, false, false, false, nil) // autoAck=true
	failOnError(err, "Falló registrar consumidor para LCP")

	forever := make(chan bool)
	go func() {
		for d := range msgsFromLCP {
			log.Printf("SNP: Recibido evento de LCP (RK: %s): %s", d.RoutingKey, string(d.Body))
			var eventoGeneral EventoLCPGeneral
			errUnmarshal := json.Unmarshal(d.Body, &eventoGeneral)
			if errUnmarshal != nil {
				log.Printf("SNP: Error decodificar evento general LCP: %v. Mensaje: %s", errUnmarshal, string(d.Body))
				continue
			}

			var notifParaEntrenador NotificacionParaEntrenador
			var enviarNotif bool = true
			var targetRoutingKey string = "" // Routing key para publicar la notificación

			switch eventoGeneral.TipoEvento {
			case "inscripcion_exitosa":
				var datosInsc DatosInscripcionEvento
				// Re-serializar y deserializar el campo Datos si es necesario
				datosBytes, _ := json.Marshal(eventoGeneral.Datos)
				json.Unmarshal(datosBytes, &datosInsc)

				notifParaEntrenador = NotificacionParaEntrenador{
					TipoNotificacion: "CONFIRMACION_INSCRIPCION",
					Mensaje:          fmt.Sprintf("¡Felicidades %s! Tu inscripción al torneo %s (%s) fue exitosa.", datosInsc.EntrenadorNombre, datosInsc.TorneoID, datosInsc.RegionTorneo),
					EntrenadorID:     datosInsc.EntrenadorID, TorneoID: datosInsc.TorneoID, RegionTorneo: datosInsc.RegionTorneo, Timestamp: time.Now().Format(time.RFC3339),
				}
				targetRoutingKey = fmt.Sprintf("notify.entrenador.%s", datosInsc.EntrenadorID)

			case "inscripcion_rechazada_suspension", "inscripcion_rechazada_expulsado":
				var datosRechazo DatosInscripcionEvento
				datosBytes, _ := json.Marshal(eventoGeneral.Datos)
				json.Unmarshal(datosBytes, &datosRechazo)
				
				tipoNotif := "RECHAZO_INSCRIPCION_SUSPENSION"
				if eventoGeneral.TipoEvento == "inscripcion_rechazada_expulsado" {
					tipoNotif = "RECHAZO_INSCRIPCION_EXPULSADO"
				}
				notifParaEntrenador = NotificacionParaEntrenador{
					TipoNotificacion: tipoNotif,
					Mensaje:          fmt.Sprintf("%s, tu intento de inscripción al torneo %s fue rechazado. %s", datosRechazo.EntrenadorNombre, datosRechazo.TorneoID, datosRechazo.MensajeAdicional),
					EntrenadorID:     datosRechazo.EntrenadorID, TorneoID: datosRechazo.TorneoID, Timestamp: time.Now().Format(time.RFC3339),
				}
				targetRoutingKey = fmt.Sprintf("notify.entrenador.%s", datosRechazo.EntrenadorID)
			
			case "nuevo_torneo_disponible": // <<<--- NUEVO MANEJO
				var datosNuevoTorneo DatosNuevoTorneoEvento
				datosBytes, _ := json.Marshal(eventoGeneral.Datos) // Re-serializar el interface{}
				errUnmarshalDatos := json.Unmarshal(datosBytes, &datosNuevoTorneo) // Deserializar en el struct específico
				if errUnmarshalDatos != nil {
					log.Printf("SNP: Error decodificar datos de nuevo torneo: %v", errUnmarshalDatos)
					enviarNotif = false
				} else {
					notifParaEntrenador = NotificacionParaEntrenador{
						TipoNotificacion: "NUEVO_TORNEO_DISPONIBLE",
						Mensaje:          fmt.Sprintf("¡Nuevo torneo disponible! ID: %s en la región %s. Creado: %s", datosNuevoTorneo.TorneoID, datosNuevoTorneo.RegionTorneo, datosNuevoTorneo.FechaCreacion),
						TorneoID:         datosNuevoTorneo.TorneoID,
						RegionTorneo:     datosNuevoTorneo.RegionTorneo,
						Timestamp:        time.Now().Format(time.RFC3339),
						// EntrenadorID se omite para que sea una notificación general
					}
					// Publicar a un routing key general para nuevos torneos
					targetRoutingKey = routingKeySNPNuevoTorneo
				}
			
			default:
				log.Printf("SNP: Tipo de evento LCP desconocido o no manejado: '%s'", eventoGeneral.TipoEvento)
				enviarNotif = false
			}

			if enviarNotif && targetRoutingKey != "" {
				publicarNotificacion(ch, notifParaEntrenador, targetRoutingKey)
			}
		}
	}()
	log.Printf("SNP: [*] Esperando mensajes de LCP. CTRL+C para salir.")
	<-forever
}
