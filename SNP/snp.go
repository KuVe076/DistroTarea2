package main

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/streadway/amqp"
)

const (
    rabbitMQURL          = "amqp://guest:guest@dist123:5672/" // URL de conexión a RabbitMQ
    lcpEventsExchange    = "lcp_events_exchange"                // Exchange donde la LCP publica eventos generales
    snpConsumesQueueName = "snp_consumes_lcp_events_queue"      // Cola donde SNP consume eventos de la LCP
    routingKeyLCPEvent   = "lcp.event"                         // Routing key para eventos generales de la LCP
    snpPublishesExchange = "snp_publishes_notifications_exchange" // Exchange donde SNP publica notificaciones a los clientes
    routingKeySNPNuevoTorneo = "notify.torneo.nuevo_disponible"   // Routing key para notificar nuevos torneos disponibles
)


// EventoLCPGeneral representa la estructura de un evento general publicado por la LCP.
// Incluye el tipo de evento, los datos asociados (pueden ser de cualquier tipo) y un timestamp.
type EventoLCPGeneral struct {
    TipoEvento string      `json:"tipo_evento"`   // Tipo de evento generado por la LCP (ej: inscripcion_exitosa, nuevo_torneo_disponible)
    Datos      interface{} `json:"datos"`         // Datos asociados al evento (estructura variable según el tipo de evento)
    Timestamp  string      `json:"timestamp"`     // Momento en que se generó el evento (formato RFC3339)
}


// DatosInscripcionEvento representa la estructura de los datos de un evento de inscripción en un torneo.
// Incluye el ID y nombre del entrenador, el ID del torneo, la región (opcional) y un mensaje adicional (opcional).
type DatosInscripcionEvento struct {
    EntrenadorID     string `json:"entrenador_id"`           // ID único del entrenador
    EntrenadorNombre string `json:"entrenador_nombre"`       // Nombre del entrenador
    TorneoID         string `json:"torneo_id"`               // ID del torneo al que se refiere el evento
    RegionTorneo     string `json:"region_torneo,omitempty"` // Región del torneo (opcional)
    MensajeAdicional string `json:"mensaje_adicional,omitempty"` // Mensaje adicional (opcional)
}


// DatosNuevoTorneoEvento representa la estructura de los datos de un evento de creación de un nuevo torneo.
// Incluye el ID del torneo, la región donde se realiza y la fecha de creación.
type DatosNuevoTorneoEvento struct {
    TorneoID      string `json:"torneo_id"`      // ID único del torneo creado
    RegionTorneo  string `json:"region_torneo"`  // Región donde se realiza el torneo
    FechaCreacion string `json:"fecha_creacion"` // Fecha y hora de creación del torneo (formato RFC3339)
}


// NotificacionParaEntrenador representa la estructura de una notificación enviada a un entrenador.
// Incluye el tipo de notificación, mensaje, identificadores relevantes y timestamp.
type NotificacionParaEntrenador struct {
    TipoNotificacion string `json:"tipo_notificacion"`      // Tipo de notificación (ej: CONFIRMACION_INSCRIPCION, CAMBIO_RANKING)
    Mensaje          string `json:"mensaje"`                // Mensaje descriptivo para el entrenador
    EntrenadorID     string `json:"entrenador_id,omitempty"`// ID del entrenador destinatario (opcional)
    TorneoID         string `json:"torneo_id,omitempty"`    // ID del torneo relacionado (opcional)
    RegionTorneo     string `json:"region_torneo,omitempty"`// Región del torneo (opcional)
    Timestamp        string `json:"timestamp"`              // Momento en que se genera la notificación (formato RFC3339)
}

// DatosCambioRanking representa la estructura de los datos de un evento de cambio de ranking de un entrenador.
// Incluye el ID y nombre del entrenador, ranking anterior y nuevo, resultado del combate, nombre del oponente, torneo y combate.
type DatosCambioRanking struct {
    EntrenadorID     string `json:"entrenador_id"`      // ID único del entrenador
    EntrenadorNombre string `json:"entrenador_nombre"`  // Nombre del entrenador
    RankingAnterior  int    `json:"ranking_anterior"`   // Ranking antes del combate
    RankingNuevo     int    `json:"ranking_nuevo"`      // Ranking después del combate
    ResultadoCombate string `json:"resultado_combate"`  // Resultado del combate ("victoria" o "derrota")
    OponenteNombre   string `json:"oponente_nombre"`    // Nombre del oponente en el combate
    TorneoID         string `json:"torneo_id"`          // ID del torneo donde ocurrió el combate
    CombateID        string `json:"combate_id"`         // ID único del combate
}

// failOnError es una función auxiliar que detiene la ejecución del programa si ocurre un error grave.
// Imprime un mensaje descriptivo junto con el error y finaliza el proceso.
func failOnError(err error, msg string) {
    if err != nil {
        log.Fatalf("SNP: %s: %s", msg, err)
    }
}

// publicarNotificacion serializa y publica una notificación para un entrenador en RabbitMQ.
// Recibe el canal de RabbitMQ, la notificación a enviar y la routing key destino.
// Si ocurre un error al serializar o publicar, lo informa por log.
func publicarNotificacion(ch *amqp.Channel, notificacion NotificacionParaEntrenador, routingKey string) {
    cuerpo, errMarshal := json.Marshal(notificacion)
    if errMarshal != nil {
        log.Printf("SNP: Error serializar notificación: %v", errMarshal)
        return
    }

    errPublish := ch.Publish(
        snpPublishesExchange, 
        routingKey,           
        false, false,
        amqp.Publishing{ContentType: "application/json", Body: cuerpo})
    if errPublish != nil {
        log.Printf("SNP: Error publicar notif (RK:%s): %v", routingKey, errPublish)
    } else {
        log.Printf("SNP: Notificación '%s' publicada (RK: %s)", notificacion.TipoNotificacion, routingKey)
    }
}

// main es el punto de entrada del servicio SNP (Sistema de Notificaciones Públicas).
// Se conecta a RabbitMQ, declara los exchanges y colas necesarios, y lanza un consumidor
// que procesa eventos publicados por la LCP para generar notificaciones a los clientes.
func main() {
    log.SetFlags(log.Ltime | log.Lshortfile)
    log.Println("SNP: Iniciando...")

    // Conexión a RabbitMQ
    var conn *amqp.Connection
    var err error
    conn, err = amqp.Dial(rabbitMQURL)
    failOnError(err, "Falló conectar RabbitMQ")
    defer conn.Close()
    log.Println("SNP: Conectado a RabbitMQ.")

    // Creación de canal
    var ch *amqp.Channel
    ch, err = conn.Channel()
    failOnError(err, "Falló abrir canal")
    defer ch.Close()

    // Declaración de exchange para eventos de la LCP
    err = ch.ExchangeDeclare(lcpEventsExchange, "direct", true, false, false, false, nil)
    failOnError(err, "Falló declarar exchange LCP")

    // Declaración de la cola donde SNP consumirá eventos de la LCP
    var qConsume amqp.Queue
    qConsume, err = ch.QueueDeclare(snpConsumesQueueName, true, false, false, false, nil)
    failOnError(err, fmt.Sprintf("Falló declarar cola '%s'", snpConsumesQueueName))

    // Bind de la cola al exchange de eventos de la LCP
    err = ch.QueueBind(qConsume.Name, routingKeyLCPEvent, lcpEventsExchange, false, nil)
    failOnError(err, fmt.Sprintf("Falló bindear cola '%s' a exchange '%s' con RK '%s'", qConsume.Name, lcpEventsExchange, routingKeyLCPEvent))
    log.Printf("SNP: Cola '%s' bindeada para eventos de LCP (RK: %s).", qConsume.Name, routingKeyLCPEvent)

    // Declaración de exchange para publicar notificaciones a los clientes
    err = ch.ExchangeDeclare(snpPublishesExchange, "direct", true, false, false, false, nil)
    failOnError(err, "Falló declarar exchange de notificaciones SNP")
    log.Printf("SNP: Exchange de notificaciones '%s' declarado.", snpPublishesExchange)

    // Registro del consumidor para recibir mensajes de la LCP
    var msgsFromLCP <-chan amqp.Delivery
    msgsFromLCP, err = ch.Consume(qConsume.Name, "", true, false, false, false, nil)
    failOnError(err, "Falló registrar consumidor para LCP")

    forever := make(chan bool)

    // Goroutine que procesa los eventos recibidos de la LCP
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
            var targetRoutingKey string = ""

            // Procesa el tipo de evento recibido y arma la notificación correspondiente
            switch eventoGeneral.TipoEvento {
            case "inscripcion_exitosa":
                // Notifica inscripción exitosa a un entrenador
                var datosInsc DatosInscripcionEvento
                datosBytes, _ := json.Marshal(eventoGeneral.Datos)
                json.Unmarshal(datosBytes, &datosInsc)

                notifParaEntrenador = NotificacionParaEntrenador{
                    TipoNotificacion: "CONFIRMACION_INSCRIPCION",
                    Mensaje:          fmt.Sprintf("¡Felicidades %s! Tu inscripción al torneo %s (%s) fue exitosa.", datosInsc.EntrenadorNombre, datosInsc.TorneoID, datosInsc.RegionTorneo),
                    EntrenadorID:     datosInsc.EntrenadorID, TorneoID: datosInsc.TorneoID, RegionTorneo: datosInsc.RegionTorneo, Timestamp: time.Now().Format(time.RFC3339),
                }
                targetRoutingKey = fmt.Sprintf("notify.entrenador.%s", datosInsc.EntrenadorID)

            case "inscripcion_rechazada_suspension", "inscripcion_rechazada_expulsado":
                // Notifica rechazo de inscripción por suspensión o expulsión
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

            case "nuevo_torneo_disponible":
                // Notifica a todos los clientes sobre un nuevo torneo disponible
                var datosNuevoTorneo DatosNuevoTorneoEvento
                datosBytes, _ := json.Marshal(eventoGeneral.Datos)
                errUnmarshalDatos := json.Unmarshal(datosBytes, &datosNuevoTorneo)
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
                    }
                    targetRoutingKey = routingKeySNPNuevoTorneo
                }

            case "cambio_ranking":
                // Notifica al entrenador sobre un cambio de ranking tras un combate
                var datosRanking DatosCambioRanking
                datosBytes, _ := json.Marshal(eventoGeneral.Datos)
                errUnmarshalDatos := json.Unmarshal(datosBytes, &datosRanking)
                if errUnmarshalDatos != nil {
                    log.Printf("SNP: Error decodificar datos de cambio de ranking: %v", errUnmarshalDatos)
                    enviarNotif = false
                } else {
                    mensaje := fmt.Sprintf("¡Actualización de ranking! Tu nuevo ranking es %d tras tu %s contra %s en el torneo %s (combate %s).",
                        datosRanking.RankingNuevo, datosRanking.ResultadoCombate, datosRanking.OponenteNombre, datosRanking.TorneoID, datosRanking.CombateID)
                    notifParaEntrenador = NotificacionParaEntrenador{
                        TipoNotificacion: "CAMBIO_RANKING",
                        Mensaje:          mensaje,
                        EntrenadorID:     datosRanking.EntrenadorID,
                        TorneoID:         datosRanking.TorneoID,
                        Timestamp:        time.Now().Format(time.RFC3339),
                    }
                    targetRoutingKey = fmt.Sprintf("notify.entrenador.%s", datosRanking.EntrenadorID)
                }

            default:
                // Si el tipo de evento no es reconocido, no se envía notificación
                log.Printf("SNP: Tipo de evento LCP desconocido o no manejado: '%s'", eventoGeneral.TipoEvento)
                enviarNotif = false
            }

            // Publica la notificación si corresponde
            if enviarNotif && targetRoutingKey != "" {
                publicarNotificacion(ch, notifParaEntrenador, targetRoutingKey)
            }
        }
    }()

    log.Printf("SNP: [*] Esperando mensajes de LCP. CTRL+C para salir.")
    <-forever // Mantiene el proceso en ejecución
}
