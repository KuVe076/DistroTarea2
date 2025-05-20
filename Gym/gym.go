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
	"time"
)

type ResultadoCombate struct {
	TorneoID	      string `json:"torneo_id"`
	IDEntrenador1     string `json:"id_entrenador_1"`
	NombreEntrenador1 string `json:"nombre_entrenador_1"`
	IDEntrenador2 	  string `json:"id_entrenador_2"`
	NombreEntrenador2 string `json:"nombre_entrenador_2"`
	IDWinner	      string `json:"id_winner"`
	NombreWinner	  string `json:"nombre_winner"`
	Fecha			  string `json:"fecha"`
	TipoMensaje		  string `json:"tipo_mensaje"`
	CombateID 		  string `json:"combate_id"`
}

type serverGimnasio struct {
	pb.UnimplementedGimnasioServer
	nombreGimnasio  string
	region          string
	aesKey			[]byte
	rabbitMQChannel *amqp.Channel
	rabbitMQConn	*amqp.Connection
}

const []regiones{
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

func NewGimnasioServer(nombre, region string, aesKey []byte, conn *amqp.Connection, ch *amqp.Channel) *serverGimnasio {
	mrand.Seed(time.Now().UnixNano())
	return &serverGimnasio{
		nombreGimnasio:  nombre,
		region:			 region,
		aesKey:			 aesKey,
		rabbitMQChannel: ch,
		rabbitMQConn:    conn,
	}
}

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
	return ciphertext, nile
}

func (s *serverGimnasio) AsignarCombate(ctx context.Conext, req *pb.AsignarRequest) (*pb.AsignarResponse, error) {
	if req.Region != s.region {
		msg := fmt.Sprintf("Gimnasio %s no opera en la región de %s", s.nombreGimnasio, req.Region)
		log.Println(msg)
		return &pb.AsingarResponse{
			Aceptado: false,
			Mensaje: msg,
		}, nil
	}

	time.Sleep(time.Duration(3 * time.Second))

	idWinner, nombreWinner := SimularCombate(req.Entrenador1, req.Entrenador2)
	fechaCombate := time.Now().Format("19-05-2025")
	resultado := ResultadoCombate{
		TorneoID: 		   req.TorneoId,
		CombateID: 		   req.CombateId,
		IDEntrenador1: 	   req.Entrenador_1.Id,
		NombreEntrenador1: req.Entrenador_1.Name,
		IDEntrenador2:     req.Entrenador_2.Id,
		NombreEntrenador2: req.Entrenador_2.Name,
		IDWinner: 		   idWinner,
		NombreWinner:      nombreWinner,
		Fecha:			   fechaCombate,

	}

	jsonData, err := json.Marshal(resultado)
	if err != nil {
		log.Printf("Error al serializar json: %v", err)
		return &pb.AsignarResponse{
			Aceptado: false,
			Mensaje:  "Error interno",
		}, err
	}

	textoCifrado, err := encryptAES_CGM(jsonData, s.aesKey)
	if err != nil {
		log.Printf("Error al cifrar resultado: %v", err)
		return &pb.AsignarResponse{
			Aceptado: false,
			Mensaje:  "Error interno",
		}, err
	}

	mensajeCifradoBase64 := base64.StdEncoding.EncodeToString(textoCifrado)
	
	maxRetries := 3
	var publishError error
	for i := 0; i < maxRetries; i++ {
		publishError = &s.rabbitMQChannel.PublishWithContext(
			ctx,
			resultadosExchange,
			resultadosRoutingKey,
			false,
			false,
			amqp.Publishing{
				ContentType: "text/plain",
				Body:		 []byte(mensajeCifradoBase64),
				MessageId: 	 resultado.CombateID,
				AppId: 		 s.nombreGimnasio,
			}
		)
		if publishError == nil {
			break
		}

		if i < maxRetries - 1 { 
			time.Sleep(time.Duration(time.Second * 3))
		}
		
		return &pb.AsingarResponse{
			Aceptado: true,
			Mensaje:  "Combate Aceptado y preparándose para el envío por " + s.nombreGimnasio,
		}, nil
		
	}
}

func SimularCombate(ent1, ent2 *pb.EntrenadorInfo) (idWinner string, NombreWinner string) {
	diff := float64(ent1.Ranking - ent2.Ranking)
	k := 100.0
	prob := 1.0 / (1.0 + math.Exp(-diff/k))

	if mrand.Float64() <= prob{
		return ent1.Id, ent1.Name
	}
	return ent2.Id, ent2.Name
}

func ConnectToRabbitMQ(url string) (*amqp.Connection, *amqp.Channel, error) {
	conn, err := amqp.Dial(url)
	if err != nil {
		return nil, nil, fmt.Errorf("Fallo al conectar a RabbitMQ: %v", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Error("Fallo al abrir canal: %v", err)
	}

	err = ch.ExchangeDeclare(
		resultadosExchange,
		"direct",
		true,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		conn.Close()
		return nil, nil, fmt.Error("Fallo al declarar Exchange: %v", err)
	}

	return conn, ch, nil
}

func IniciarServerGimnasio(nombreGimnasio string, region string, aesKey []byte, chRabbit *amqp.Channel, port string) {
	lis, err := net.listen("tcp", port)
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

func main() {
	PassKanto := "SushiSashimiTempuraWasabiMirin"
	PassJohto := "RamenUdonSobaYakitoriTonkatsu"
	PassHoenn := "MisoDashiShoyuGariSakeChawan"
	PassSinnoh := "TeriyakiOkonomiyakiGyozaUnagi"
	PassTeselia := "MatchaSakuraMochiAnkoDangoYuzu"
	PassKalos := "KatsuCurryDonburiNattoEdamame"
	PassAlola := "TeppanyakiFuguOnigiriTsukune"
	PassGalar := "NigiriMakiGunkanInariTamagoDo"
	PassPaldea := "BentoBoxShabuKushikatsuKinpira"
	
	keyKanto := []byte(passKanto)
	keyJohto := []byte(passJohto)
	keyHoenn := []byte(passHoenn)
	keySinnoh := []byte(passSinnoh)
	keyTeselia := []byte(passTeselia)
	keyKalos := []byte(passKalos)
	keyAlola := []byte(passAlola)
	keyGalar := []byte(passGalar)
	keyPaldea := []byte(passPaldea)
	
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

	for i := 0; i < 9; i++ {
		
	}


}




