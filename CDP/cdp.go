package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/streadway/amqp"
)

const (
	// URL de conexión a RabbitMQ (usuario: guest, password: guest, host: rabbitmq, puerto: 5672)
	rabbitURL = "amqp://guest:guest@dist123:5672/"

	// Nombre del exchange donde los gimnasios publican los resultados de los combates
	resultadosGymExchange = "resultados_exchange"

	// Nombre de la cola donde el CDP recibe los resultados de los combates
	colaResultadosCDP = "cdp_resultados_cola"

	// Routing key utilizada por los gimnasios para enviar resultados de combates
	routingKeyGym = "resultados.combate"

	// Exchange donde el CDP publica los resultados validados para el LCP
	cdpPublicaResultadosExchange = "cdp_validados_exchange"

	// Routing key utilizada para enviar resultados validados al LCP
	routingKeyLCP = "resultado.validado.lcp"
)

// ResultadoCombate representa la estructura de un resultado de combate recibido desde un gimnasio.
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type ResultadoCombate struct {
	TorneoID          string `json:"torneo_id"`           // Identificador del torneo
	IDEntrenador1     string `json:"id_entrenador_1"`     // ID del primer entrenador
	NombreEntrenador1 string `json:"nombre_entrenador_1"` // Nombre del primer entrenador
	IDEntrenador2     string `json:"id_entrenador_2"`     // ID del segundo entrenador
	NombreEntrenador2 string `json:"nombre_entrenador_2"` // Nombre del segundo entrenador
	IDWinner          string `json:"id_winner"`           // ID del entrenador ganador
	NombreWinner      string `json:"nombre_winner"`       // Nombre del entrenador ganador
	Fecha             string `json:"fecha"`               // Fecha del combate
	TipoMensaje       string `json:"tipo_mensaje"`        // Tipo de mensaje (por ejemplo, "resultado")
	CombateID         string `json:"combate_id"`          // Identificador único del combate
}

// CDP representa el Centro de Datos Pokémon (CDP).
// Esta estructura contiene:
// - rabbitCh: el canal de comunicación con RabbitMQ para enviar y recibir mensajes.
// - AESKeys: un mapa que asocia el nombre de cada gimnasio con su clave AES para desencriptar los mensajes recibidos.
type CDP struct {
    rabbitCh *amqp.Channel
    AESKeys  map[string][]byte
}

// NewCDP es una función constructora que crea una nueva instancia de la estructura CDP.
// Recibe un canal de RabbitMQ y un mapa con las llaves AES de los gimnasios.
// Retorna un puntero a la estructura CDP inicializada.
func NewCDP(ch *amqp.Channel, keys map[string][]byte) *CDP {
    return &CDP{
        rabbitCh: ch,
        AESKeys:  keys,
    }
}

// Función para descifrar un mensaje cifrado en base64 usando AES en modo GCM.
// Recibe el texto cifrado en base64 y la clave AES correspondiente.
// Devuelve el texto plano descifrado o un error si ocurre algún problema.
func decryptAES_GCM(ciphertextBase64 string, key []byte) ([]byte, error) {
    // Decodifica el texto base64 a bytes
    ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
    if err != nil {
        return nil, fmt.Errorf("fallo al decodificar base64: %w", err)
    }
    // Crea un nuevo bloque de cifrado AES con la clave proporcionada
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, fmt.Errorf("fallo al crear cipher: %w", err)
    }
    // Inicializa el modo GCM (Galois/Counter Mode) para el bloque AES
    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, fmt.Errorf("fallo al crear GCM: %w", err)
    }
    nonceSize := gcm.NonceSize()
    // Verifica que el texto cifrado tenga al menos el tamaño del nonce
    if len(ciphertext) < nonceSize {
        return nil, fmt.Errorf("texto cifrado demasiado corto")
    }
    // Separa el nonce y el texto cifrado real
    nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
    // Descifra el mensaje usando GCM
    plaintext, err := gcm.Open(nil, nonce, actualCiphertext, nil)
    if err != nil {
        return nil, fmt.Errorf("fallo al descifrar (GCM open): %w", err)
    }
    return plaintext, nil
}

// ProcesarMensajeGym procesa un mensaje recibido desde un gimnasio.
// 1. Obtiene la clave AES correspondiente al gimnasio usando el AppId del mensaje.
// 2. Desencripta el cuerpo del mensaje usando AES-GCM.
// 3. Deserializa el JSON desencriptado a la estructura ResultadoCombate.
// 4. Publica el resultado validado en el exchange correspondiente para el LCP.
// 5. Maneja errores de desencriptado, deserialización y publicación, notificando a RabbitMQ según corresponda.
func (cdp *CDP) ProcesarMensajeGym(d amqp.Delivery) {
    gymKey := cdp.AESKeys[d.AppId] // Obtiene la clave AES del gimnasio

    plaintext, err := decryptAES_GCM(string(d.Body), gymKey) // Desencripta el mensaje
    if err != nil {
        log.Printf("Error al desencriptar la llave. %v", err)
        d.Nack(false, false) // Notifica fallo sin reintentar
        return
    }

    var resultado ResultadoCombate
    if err := json.Unmarshal(plaintext, &resultado); err != nil { // Deserializa el JSON
        log.Printf("Error al deserializar combate: %v", err)
        d.Nack(false, false) // Notifica fallo sin reintentar
        return
    }

    // Publica el resultado validado al exchange para el LCP, con timeout de 5 segundos
    _, pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer pubCancel()
    publishError := cdp.rabbitCh.Publish(
        cdpPublicaResultadosExchange,
        routingKeyLCP,
        false, false,
        amqp.Publishing{
            ContentType: "application/json",
            Body:        plaintext,
            MessageId:   resultado.CombateID,
            AppId:       "CDP",
            Timestamp:   time.Now(),
        },
    )

    if publishError != nil {
        log.Printf("Error al publicar resultado: %v", publishError)
        d.Nack(false, true) // Notifica fallo y reintenta
        return
    }

    log.Printf("Resultado validado del Combate %s. Enviado a LCP", resultado.CombateID)
    d.Ack(false) // Confirma procesamiento exitoso
}

// stringToAESKeyBytes convierte un string a un slice de bytes para usarlo como clave AES.
// s: string que representa la clave.
// expectedSize: tamaño esperado de la clave en bytes.
// Retorna la clave como slice de bytes y un error (si ocurriera).
func stringToAESKeyBytes(s string, expectedSize int) ([]byte, error) {
    keyBytes := []byte(s)
    return keyBytes, nil
}

func main() {
    log.Println("Iniciando Centro de Datos Pokemon...")

    // Mapa con los nombres de los gimnasios y sus respectivas claves AES en texto plano
    GymKeys := map[string]string{
        "Gimnasio Agua":   "SushiSashimiTempuraWasabiMirinXY",
        "Gimnasio Tierra": "RamenUdonSobaYakitoriTonkatsu12",
    	"Gimnasio Fuego":  "MisoDashiShoyuGariSakeChawanABCD",
        "Gimnasio Viento": "TeriyakiOkonomiyakiGyozaUnagiQW",
        "Gimnasio Éter":   "MatchaSakuraMochiAnkoDangoYuzuZ",
        "Gimnasio Sopa":   "KatsuCurryDonburiNattoEdamameER",
        "Gimnasio Baba":   "TeppanyakiFuguOnigiriTsukuneTYU",
        "Gimnasio Madera": "NigiriMakiGunkanInariTamagoDoAS",
        "Gimnasio Veneno": "BentoBoxShabuKushikatsuKinpiraF",
    }

    // Convierte las claves de los gimnasios a slices de bytes para AES
    GymAESKeys := make(map[string][]byte)
    for gymName, pass := range GymKeys {
        key, err := stringToAESKeyBytes(pass, 32)
        if err != nil {
            log.Fatalf("Error con clave para %s", gymName)
        }
        GymAESKeys[gymName] = key
    }

    // Conexión a RabbitMQ
    connRabbit, err := amqp.Dial(rabbitURL)
    if err != nil {
        log.Fatalf("Error al conectar a Rabbit: %v", err)
    }
    defer connRabbit.Close()

    // Creación de canal de RabbitMQ
    chRabbit, err := connRabbit.Channel()
    if err != nil {
        log.Fatalf("Error al abrir canal de Rabbit: %v", err)
    }
    defer chRabbit.Close()

    // Declaración del exchange donde los gimnasios publican resultados
    err = chRabbit.ExchangeDeclare(resultadosGymExchange, "direct", true, false, false, false, nil)
    if err != nil {
        log.Fatalf("Error al definir exchange para gimnasios")
    }

    // Declaración de la cola donde el CDP recibirá los resultados
    q, err := chRabbit.QueueDeclare(colaResultadosCDP, true, false, false, false, nil)
    if err != nil {
        log.Fatalf("Error al declarar cola resultados: %v", err)
    }

    // Enlaza la cola al exchange usando la routing key de los gimnasios
    err = chRabbit.QueueBind(q.Name, routingKeyGym, resultadosGymExchange, false, nil)
    if err != nil {
        log.Fatalf("Fallo al enlazar cola: %v", err)
    }

    // Declaración del exchange donde el CDP publica resultados validados
    err = chRabbit.ExchangeDeclare(cdpPublicaResultadosExchange, "direct", true, false, false, false, nil)
    if err != nil {
        log.Fatalf("Error al definir exchange LCP: %v", err)
    }

    // Instancia el CDP con el canal de RabbitMQ y las claves AES
    cdpInstance := NewCDP(chRabbit, GymAESKeys)

    // Se suscribe a la cola para recibir mensajes de resultados
    msgs, err := chRabbit.Consume(q.Name, "cdp_consumer", false, false, false, false, nil)
    if err != nil {
        log.Fatalf("Fallo al registrar consumidor: %v", err)
    }

    var wg sync.WaitGroup
    // Procesa los mensajes recibidos de forma concurrente
    go func() {
        for d := range msgs {
            wg.Add(1)
            go func(delivery amqp.Delivery) {
                defer wg.Done()
                cdpInstance.ProcesarMensajeGym(delivery)
            }(d)
        }
        log.Println("CDP: El canal de mensajes de Rabbit cerró")
    }()

    log.Println("Esperando resultados. Para salir presiona Ctrl+C")
    // Espera señal de interrupción (Ctrl+C) para cerrar el programa
    stopChan := make(chan os.Signal, 1)
    signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)
    <-stopChan

    log.Println("Cerrando...")

    // Espera a que todos los mensajes en proceso terminen antes de cerrar
    c := make(chan struct{})
    go func() {
        defer close(c)
        wg.Wait()
    }()

    select {
    case <-c:
        log.Println("Todos los procesadores de mensajes han terminado")
    case <-time.After(10 * time.Second):
        log.Println("Timeout durante el cierre")
    }

    log.Println("CDP terminado")
}
