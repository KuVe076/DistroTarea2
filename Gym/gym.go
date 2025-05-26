package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	mrand "math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	pb "DISTROTAREA2/proto"

	amqp "github.com/streadway/amqp"

	"google.golang.org/grpc"
)

const (
    rabbitURL            = "amqp://guest:guest@rabbitmq:5672/" // URL de conexión a RabbitMQ
    resultadosExchange   = "resultados_exchange"               // Nombre del exchange donde se publican los resultados de los combates
    resultadosRoutingKey = "resultados.combate"                // Routing key utilizada para los mensajes de resultados de combate
)

// ResultadoCombate representa la estructura de los datos de un combate realizado en el gimnasio.
// Cada campo tiene una etiqueta JSON para facilitar la serialización/deserialización.
type ResultadoCombate struct {
    TorneoID          string `json:"torneo_id"`           // ID del torneo al que pertenece el combate
    IDEntrenador1     string `json:"id_entrenador_1"`     // ID del primer entrenador participante
    NombreEntrenador1 string `json:"nombre_entrenador_1"` // Nombre del primer entrenador
    IDEntrenador2     string `json:"id_entrenador_2"`     // ID del segundo entrenador participante
    NombreEntrenador2 string `json:"nombre_entrenador_2"` // Nombre del segundo entrenador
    IDWinner          string `json:"id_winner"`           // ID del entrenador ganador
    NombreWinner      string `json:"nombre_winner"`       // Nombre del entrenador ganador
    Fecha             string `json:"fecha"`               // Fecha en la que se realizó el combate
    TipoMensaje       string `json:"tipo_mensaje"`        // Tipo de mensaje (puede ser usado para distinguir eventos)
    CombateID         string `json:"combate_id"`          // ID único del combate
}

// serverGimnasio representa el servidor gRPC de un gimnasio Pokémon.
// Contiene la lógica y recursos necesarios para procesar combates y publicar resultados en RabbitMQ.
type serverGimnasio struct {
    pb.UnimplementedGimnasioServer // Embebe la implementación no utilizada del servidor gRPC generado por protobuf
    nombreGimnasio  string         // Nombre del gimnasio (ej: Gimnasio Agua)
    region          string         // Región en la que opera el gimnasio (ej: Kanto)
    aesKey          []byte         // Clave AES utilizada para cifrar los resultados de combate
    rabbitMQChannel *amqp.Channel  // Canal de RabbitMQ para publicar mensajes
    rabbitMQConn    *amqp.Connection // Conexión a RabbitMQ
}

// NewGimnasioServer es una función constructora que crea una nueva instancia del servidor de gimnasio Pokémon.
// Recibe el nombre y la región del gimnasio, la clave AES, la conexión y el canal de RabbitMQ.
// Retorna un puntero a la estructura serverGimnasio inicializada.
func NewGimnasioServer(nombre, region string, aesKey []byte, conn *amqp.Connection, ch *amqp.Channel) *serverGimnasio {
    mrand.Seed(time.Now().UnixNano())
    return &serverGimnasio{
        nombreGimnasio:  nombre,
        region:          region,
        aesKey:          aesKey,
        rabbitMQChannel: ch,
        rabbitMQConn:    conn,
    }
}

// encryptAES_CGM cifra un mensaje usando AES en modo GCM (Galois/Counter Mode).
// Recibe el texto plano y la clave AES, y retorna el texto cifrado (con el nonce prepended) o un error.
func encryptAES_CGM(plaintext []byte, key []byte) ([]byte, error) {
    block, err := aes.NewCipher(key)
    if err != nil {
        return nil, err
    }

    gcm, err := cipher.NewGCM(block)
    if err != nil {
        return nil, err
    }

    nonce := make([]byte, gcm.NonceSize())
    if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
        return nil, err
    }

    ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
    return ciphertext, nil
}

// AsignarCombate es el método gRPC que recibe la solicitud de asignar un combate a este gimnasio.
// Verifica si la región es correcta, simula el combate, cifra el resultado y lo publica en RabbitMQ.
// Devuelve una respuesta indicando si el combate fue aceptado y procesado correctamente.
func (s *serverGimnasio) AsignarCombate(ctx context.Context, req *pb.AsignarCombateRequest) (*pb.AsignarCombateResponse, error) {
    // Verifica si la región del combate corresponde a la del gimnasio
    if req.Region != s.region {
        msg := fmt.Sprintf("Gimnasio %s no opera en la región de %s", s.nombreGimnasio, req.Region)
        log.Println(msg)
        return &pb.AsignarCombateResponse{
            Aceptado: false,
            Mensaje:  msg,
        }, nil
    }

    // Simula un retardo para representar el tiempo de combate
    time.Sleep(time.Duration(3 * time.Second))

    // Simula el combate y obtiene el ganador
    idWinner, nombreWinner := SimularCombate(req.Entrenador_1, req.Entrenador_2)
    fechaCombate := time.Now().Format("2006-01-02")
    resultado := ResultadoCombate{
        TorneoID:          req.TorneoId,
        CombateID:         req.CombateId,
        IDEntrenador1:     req.Entrenador_1.Id,
        NombreEntrenador1: req.Entrenador_1.Name,
        IDEntrenador2:     req.Entrenador_2.Id,
        NombreEntrenador2: req.Entrenador_2.Name,
        IDWinner:          idWinner,
        NombreWinner:      nombreWinner,
        Fecha:             fechaCombate,
    }

    // Serializa el resultado a JSON
    jsonData, err := json.Marshal(resultado)
    if err != nil {
        log.Printf("Error al serializar json: %v", err)
        return &pb.AsignarCombateResponse{
            Aceptado: false,
            Mensaje:  "Error interno",
        }, err
    }

    // Cifra el resultado usando AES-GCM
    textoCifrado, err := encryptAES_CGM(jsonData, s.aesKey)
    if err != nil {
        log.Printf("Error al cifrar resultado: %v", err)
        return &pb.AsignarCombateResponse{
            Aceptado: false,
            Mensaje:  "Error interno",
        }, err
    }

    // Codifica el mensaje cifrado en base64 para enviarlo por RabbitMQ
    mensajeCifradoBase64 := base64.StdEncoding.EncodeToString(textoCifrado)

    // Publica el mensaje cifrado en RabbitMQ, con reintentos en caso de error
    maxRetries := 3
    var publishError error
    for i := 0; i < maxRetries; i++ {
        publishError = s.rabbitMQChannel.Publish(
            resultadosExchange,
            resultadosRoutingKey,
            false,
            false,
            amqp.Publishing{
                ContentType: "application/json",
                Body:        []byte(mensajeCifradoBase64),
                MessageId:   resultado.CombateID,
                AppId:       s.nombreGimnasio,
            },
        )
        if publishError == nil {
            log.Printf(" %s: Resultado del %s enviado exitosamente a RabbitMQ.", s.nombreGimnasio, resultado.CombateID)
            break
        }

        // Si falla, espera antes de reintentar
        if i < maxRetries-1 {
            time.Sleep(time.Duration(time.Second * 3))
        }

    }

    // Devuelve la respuesta de aceptación del combate
    return &pb.AsignarCombateResponse{
        Aceptado: true,
        Mensaje:  "Combate Aceptado y preparándose para el envío por " + s.nombreGimnasio,
    }, nil
}

// SimularCombate simula el resultado de un combate entre dos entrenadores usando sus rankings.
// Calcula la probabilidad de victoria del primer entrenador usando una función logística basada en la diferencia de ranking.
// Retorna el ID y el nombre del entrenador ganador.
func SimularCombate(ent1, ent2 *pb.EntrenadorInfo) (idWinner string, NombreWinner string) {
    diff := float64(ent1.Ranking - ent2.Ranking)
    k := 100.0
    prob := 1.0 / (1.0 + math.Exp(-diff/k))

    if mrand.Float64() <= prob {
        return ent1.Id, ent1.Name
    }
    return ent2.Id, ent2.Name
}

// ConnectToRabbitMQ realiza la conexión y configuración inicial a RabbitMQ.
// Retorna la conexión, el canal y un error si ocurre algún problema.
func ConnectToRabbitMQ(url string) (*amqp.Connection, *amqp.Channel, error) {
    conn, err := amqp.Dial(url)
    if err != nil {
        // Retorna error si falla la conexión a RabbitMQ
        return nil, nil, fmt.Errorf("fallo al conectar a RabbitMQ: %v", err)
    }

    ch, err := conn.Channel()
    if err != nil {
        // Cierra la conexión si falla la apertura del canal
        conn.Close()
        return nil, nil, fmt.Errorf("fallo al abrir canal: %v", err)
    }

    // Declara el exchange donde se publicarán los resultados de combate
    err = ch.ExchangeDeclare(
        resultadosExchange, // Nombre del exchange
        "direct",           // Tipo de exchange
        true,               // Durable
        false,              // Auto-deleted
        false,              // Internal
        false,              // No-wait
        nil,                // Arguments
    )
    if err != nil {
        // Cierra canal y conexión si falla la declaración del exchange
        ch.Close()
        conn.Close()
        return nil, nil, fmt.Errorf("fallo al declarar Exchange: %v", err)
    }

    // Retorna la conexión y el canal listos para usar
    return conn, ch, nil
}

// IniciarServerGimnasio inicia un servidor gRPC de gimnasio en el puerto indicado.
// Registra el servicio y queda escuchando solicitudes de combates.
func IniciarServerGimnasio(nombreGimnasio string, region string, aesKey []byte, connRabbit *amqp.Connection, chRabbit *amqp.Channel, port string) {
    lis, err := net.Listen("tcp", port)
    if err != nil {
        log.Fatalf("Error al escuchar: %v", err)
    }

    grpcServer := grpc.NewServer()
    gymServer := NewGimnasioServer(nombreGimnasio, region, aesKey, connRabbit, chRabbit)

    pb.RegisterGimnasioServer(grpcServer, gymServer)
    log.Printf("Gimnasio %s escuchando en %v", nombreGimnasio, lis.Addr())
    if err := grpcServer.Serve(lis); err != nil {
        log.Printf("El Gimnasio %s fallo al servir GRPC", nombreGimnasio)
    }
}

// main inicializa las claves, regiones y servidores de los gimnasios Pokémon.
// Conecta a RabbitMQ, lanza los servidores gRPC de cada gimnasio y espera señal de cierre.
func main() {
    // Definición de contraseñas y claves AES para cada región
    passKanto := "SushiSashimiTempuraWasabiMirinXY"
    passJohto := "RamenUdonSobaYakitoriTonkatsu12"
    passHoenn := "MisoDashiShoyuGariSakeChawanABCD" 
    passSinnoh := "TeriyakiOkonomiyakiGyozaUnagiQW"
    passTeselia := "MatchaSakuraMochiAnkoDangoYuzuZ"
    passKalos := "KatsuCurryDonburiNattoEdamameER"
    passAlola := "TeppanyakiFuguOnigiriTsukuneTYU" 
    passGalar := "NigiriMakiGunkanInariTamagoDoAS" 
    passPaldea := "BentoBoxShabuKushikatsuKinpiraF" 

    keyKanto := []byte(passKanto)
    keyJohto := []byte(passJohto)
    keyHoenn := []byte(passHoenn)
    keySinnoh := []byte(passSinnoh)
    keyTeselia := []byte(passTeselia)
    keyKalos := []byte(passKalos)
    keyAlola := []byte(passAlola)
    keyGalar := []byte(passGalar)
    keyPaldea := []byte(passPaldea)

    // Listas de regiones, claves y nombres de gimnasios
    regiones := []string{
        "Kanto",
        "Johto",
        "Hoenn",
        "Sinnoh",
        "Teselia",
        "Kalos",
        "Alola",
        "Galar",
        "Paldea",
    }

    GymKeys := [][]byte{
        keyKanto,
        keyJohto,
        keyHoenn,
        keySinnoh,
        keyTeselia,
        keyKalos,
        keyAlola,
        keyGalar,
        keyPaldea,
    }

    GymNames := []string{
        "Gimnasio Agua",
        "Gimnasio Tierra",
        "Gimnasio Fuego",
        "Gimnasio Viento",
        "Gimnasio Éter",
        "Gimnasio Sopa",
        "Gimnasio Baba",
        "Gimnasio Madera",
        "Gimnasio Veneno",
    }

    // Conexión a RabbitMQ
    rabbitConn, rabbitCh, err := ConnectToRabbitMQ(rabbitURL)
    if err != nil {
        log.Fatalf("No se pudo conectar a Rabbit: %v", err)
    }

    puerto_base := 50052

    // Inicia los servidores de gimnasio en puertos consecutivos
    for i := 0; i < 9; i++ {
        puerto_actual := ":" + strconv.Itoa(puerto_base+i)
        go IniciarServerGimnasio(GymNames[i], regiones[i], GymKeys[i], rabbitConn, rabbitCh, puerto_actual)

        log.Printf("El %s está activado", GymNames[i])
    }

    // Espera señal de cierre (Ctrl+C)
    stopChan := make(chan os.Signal, 1)
    signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM)
    <-stopChan

    log.Println("Terminando gimnasios...")

    log.Println("Gimnasios Terminados")
}
