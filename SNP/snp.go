package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/streadway/amqp"
)

const (
	rabbitMQURL = "amqp://guest:guest@rabbitmq:5672/"
	lcpEventsExchange      = "lcp_events_exchange"      // Exchange del que SNP consume
	snpConsumesQueueName   = "snp_consumes_lcp_events_queue" // Cola de SNP para eventos de LCP
	routingKeyLCPEvent     = "lcp.event.inscripcion"    // Routing key que SNP escucha de LCP
	snpPublishesExchange   = "snp_publishes_notifications_exchange" // Exchange donde SNP publica para entrenadores
)

// EventoInscripcionLCP (debe ser idéntica a la definida en lcp.go)
type EventoInscripcionLCP struct {
	TipoEvento       string `json:"tipo_evento"`
	EntrenadorID     string `json:"entrenador_id"`
	EntrenadorNombre string `json:"entrenador_nombre"`
	TorneoID         string `json:"torneo_id,omitempty"`
	RegionTorneo     string `json:"region_torneo,omitempty"`
	MensajeAdicional string `json:"mensaje_adicional,omitempty"`
	Timestamp        string `json:"timestamp"`
}

// NotificacionParaEntrenador es lo que SNP envía al entrenador
type NotificacionParaEntrenador struct {
	TipoNotificacion string `json:"tipo_notificacion"` // "confirmacion_inscripcion", "rechazo_inscripcion_suspension", etc.
	Mensaje          string `json:"mensaje"`
	EntrenadorID     string `json:"entrenador_id"`
	TorneoID         string `json:"torneo_id,omitempty"`
	RegionTorneo     string `json:"region_torneo,omitempty"`
	Timestamp        string `json:"timestamp"`
}

func failOnError(err error, msg string) {
	if err != nil { log.Fatalf("SNP: %s: %s", msg, err) }
}

func main() {
	log.SetFlags(log.Ltime | log.Lshortfile); log.Println("SNP: Iniciando...")
	var conn *amqp.Connection; var err error
	conn, err = amqp.Dial(rabbitMQURL); failOnError(err, "Falló conectar RabbitMQ"); defer conn.Close()
	log.Println("SNP: Conectado a RabbitMQ.")
	var ch *amqp.Channel; ch, err = conn.Channel(); failOnError(err, "Falló abrir canal"); defer ch.Close()

	// Exchange para consumir eventos de LCP
	err = ch.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
	failOnError(err, "Falló declarar exchange LCP")

	var qConsume amqp.Queue
	qConsume, err = ch.QueueDeclare(snpConsumesQueueName, true, false, false, false, nil)
	failOnError(err, fmt.Sprintf("Falló declarar cola '%s'", snpConsumesQueueName))

	err = ch.QueueBind(qConsume.Name, routingKeyLCPEvent, lcpEventsExchange, false, nil)
	failOnError(err, fmt.Sprintf("Falló bindear cola '%s' a exchange '%s' con RK '%s'", qConsume.Name, lcpEventsExchange, routingKeyLCPEvent))
	log.Printf("SNP: Cola '%s' bindeada para eventos de LCP.", qConsume.Name)

	// Exchange para publicar notificaciones a entrenadores
	err = ch.ExchangeDeclare(snpPublishesExchange, "direct", true, false, false, false, nil)
	failOnError(err, "Falló declarar exchange de notificaciones SNP")

	var msgsFromLCP <-chan amqp.Delivery
	msgsFromLCP, err = ch.Consume(qConsume.Name, "", true, false, false, false, nil) // autoAck=true
	failOnError(err, "Falló registrar consumidor para LCP")

	forever := make(chan bool)
	go func() {
		for d := range msgsFromLCP {
			log.Printf("SNP: Recibido evento de LCP: %s", string(d.Body))
			var evento EventoInscripcionLCP
			errUnmarshal := json.Unmarshal(d.Body, &evento)
			if errUnmarshal != nil { log.Printf("SNP: Error decodificar evento LCP: %v", errUnmarshal); continue }

			var notif NotificacionParaEntrenador
			var enviarNotif bool = true

			switch evento.TipoEvento {
			case "inscripcion_exitosa":
				notif = NotificacionParaEntrenador{
					TipoNotificacion: "CONFIRMACION_INSCRIPCION",
					Mensaje: fmt.Sprintf("¡Felicidades %s! Tu inscripción al torneo %s (%s) fue exitosa.", evento.EntrenadorNombre, evento.TorneoID, evento.RegionTorneo),
					EntrenadorID: evento.EntrenadorID, TorneoID: evento.TorneoID, RegionTorneo: evento.RegionTorneo, Timestamp: time.Now().Format(time.RFC3339),
				}
			case "inscripcion_rechazada_suspension":
				notif = NotificacionParaEntrenador{
					TipoNotificacion: "RECHAZO_INSCRIPCION_SUSPENSION",
					Mensaje: fmt.Sprintf("%s, tu intento de inscripción al torneo %s fue rechazado. %s", evento.EntrenadorNombre, evento.TorneoID, evento.MensajeAdicional),
					EntrenadorID: evento.EntrenadorID, TorneoID: evento.TorneoID, Timestamp: time.Now().Format(time.RFC3339),
				}
			// Aquí se añadirían más casos para otros eventos de LCP (ej. nuevo torneo, cambio ranking, etc.)
			default:
				log.Printf("SNP: Tipo de evento LCP desconocido o no manejado: %s", evento.TipoEvento)
				enviarNotif = false
			}
			if enviarNotif { publicarNotificacionParaEntrenador(ch, notif) }
		}
	}()
	log.Printf("SNP: [*] Esperando mensajes de LCP. CTRL+C para salir.")
	<-forever
}

func publicarNotificacionParaEntrenador(ch *amqp.Channel, notificacion NotificacionParaEntrenador) {
	cuerpo, errMarshal := json.Marshal(notificacion)
	if errMarshal != nil { log.Printf("SNP: Error serializar notificación: %v", errMarshal); return }
	routingKey := fmt.Sprintf("notify.entrenador.%s", notificacion.EntrenadorID) // Routing key específico por entrenador
	errPublish := ch.Publish(snpPublishesExchange, routingKey, false, false, amqp.Publishing{ContentType: "application/json", Body: cuerpo})
	if errPublish != nil { log.Printf("SNP: Error publicar notif para %s (RK:%s): %v", notificacion.EntrenadorID, routingKey, errPublish) } else {
		log.Printf("SNP: Notificación '%s' publicada para Entrenador %s (RK: %s)", notificacion.TipoNotificacion, notificacion.EntrenadorID, routingKey)
	}
}