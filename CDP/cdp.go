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
	rabbitURL = "amqp://guest:guest@rabbitmq:5672/"
	resultadosGymExchange = "resultados_exchange"
	colaResultadosCDP = "cdp_resultados_cola"
	routingKeyGym = "resultado.combate"

	exchangeLCP =  "lcp_datos_validados_exchange"
	routingKeyLCP = "resultado.validado.lcp"
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

type CDP struct {
	rabbitCh *amqp.Channel
	AESKeys  map[string][]byte

}

func NewCDP(ch *amqp.Channel, keys map[string][]byte) *CDP {
	return &CDP{
		rabbitCh : ch,
		AESKeys: keys,
	}
}

func decryptAES_GCM(ciphertextBase64 string, key []byte) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil { return nil, fmt.Errorf("fallo al decodificar base64: %w", err) }
	block, err := aes.NewCipher(key)
	if err != nil { return nil, fmt.Errorf("fallo al crear cipher: %w", err) }
	gcm, err := cipher.NewGCM(block)
	if err != nil { return nil, fmt.Errorf("fallo al crear GCM: %w", err) }
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize { return nil, fmt.Errorf("texto cifrado demasiado corto") }
	nonce, actualCiphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, actualCiphertext, nil)
	if err != nil { return nil, fmt.Errorf("fallo al descifrar (GCM open): %w", err) }
	return plaintext, nil
}

func (cdp *CDP) ProcesarMensajeGym(d amqp.Delivery) {
	gymKey,_ := cdp.AESKeys[d.AppId]

	plaintext, err := decryptAES_GCM(string(d.Body), gymKey)
	if err != nil {
		log.Printf("Error al desencriptar la llave. %v", err)
		d.Nack(false, false)
		return
	}

	var resultado ResultadoCombate
	if err := json.Unmarshal(plaintext, &resultado); err != nil {
		log.Printf("Error al deserializar combate: %v", err)
		d.Nack(false, false)
		return
	}
    
	_ , pubCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pubCancel()
	publishError := cdp.rabbitCh.Publish(
		exchangeLCP,
		routingKeyLCP,
		false, false,
		amqp.Publishing{
			ContentType: "application/json",
			Body:		 plaintext,
			MessageId:	 resultado.CombateID,
			AppId:		 "CDP",
			Timestamp:   time.Now(),
		},
	)
	
	if publishError != nil {
		log.Printf("Error al publicar resultado: %v", publishError)
		d.Nack(false, true)
		return
	}

	log.Printf("Resultado validado del Combate %s. Enviado a LCP", resultado.CombateID)
	d.Ack(false)
}

func stringToAESKeyBytes(s string, expectedSize int) ([]byte, error) {
	keyBytes := []byte(s)
	return keyBytes, nil
}

func main() {
	log.Println("Iniciando Centro de Datos Pokemon...")

	GymKeys := map[string]string{
		"Gimnasio Agua": "SushiSashimiTempuraWasabiMirin",
		"Gimnasio Tierra": "RamenUdonSobaYakitoriTonkatsu",
		"Gimnasio Fuego": "MisoDashiShoyuGariSakeChawan",
		"Gimnasio Viento": "TeriyakiOkonomiyakiGyozaUnagi",
		"Gimnasio Éter": "MatchaSakuraMochiAnkoDangoYuzu",
		"Gimnasio Sopa": "KatsuCurryDonburiNattoEdamame",
		"Gimnasio Baba": "TeppanyakiFuguOnigiriTsukune",
		"Gimnasio Madera": "NigiriMakiGunkanInariTamagoDo",
		"Gimnasio Veneno": "BentoBoxShabuKushikatsuKinpira",
	}

	GymAESKeys := make(map[string][]byte)
	for gymName, pass := range GymKeys {
		key, err := stringToAESKeyBytes(pass, 32)
		if err != nil {
			log.Fatalf("Error con clave para %s", gymName)
		}

		GymAESKeys[gymName] = key
	}

	connRabbit, err := amqp.Dial(rabbitURL)
	if err != nil { 
		log.Fatalf("Error al conectar a Rabbit: %v", err)
	}
	defer connRabbit.Close()

	chRabbit, err := connRabbit.Channel()
	if err != nil {
		log.Fatalf("Error al abrir canal de Rabbit: %v", err)
	}

	defer chRabbit.Close()
	
	err = chRabbit.ExchangeDeclare(resultadosGymExchange, "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Error al definir exchange para gimnasios")
	}
	q, err := chRabbit.QueueDeclare(colaResultadosCDP, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Error al declarar cola resultados: %v", err)
	}

	err = chRabbit.QueueBind(q.Name, routingKeyGym, resultadosGymExchange, false, nil)
	if err != nil {
		log.Fatalf("Fallo al enlazar cola: %v", err)
	}

	err = chRabbit.ExchangeDeclare(exchangeLCP, "direct", true, false, false, false, nil)
	if err != nil { 
		log.Fatalf("Error al definir exchange LCP: %v", err)
	}

	cdpInstance := NewCDP(chRabbit, GymAESKeys)

	msgs, err := chRabbit.Consume(q.Name, "cdp_consumer", false, false, false, false, nil)
	if err != nil {
		log.Fatalf("Fallo al registrar consumidor: %v", err)
	}

	var wg sync.WaitGroup
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
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, syscall.SIGINT, syscall.SIGTERM) 
	<- stopChan

	log.Println("Cerrando...")

	c := make(chan struct{})
	go func() {
		defer close(c)
		wg.Wait()
	}()

	select {
	case <- c:
		log.Println("Todos los procesadores de mensajes han terminado")
	case <- time.After(10*time.Second):
		log.Println("Timeout durante el cierre")
	}

	log.Println("CDP terminado")
	
}
