package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "DISTROTAREA2/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Entrenador struct {
	ID               string
	Name             string
	Region           string
	Suspension       int
	TournamentStatus string
	IdTournament     string
	EsManual         bool
	mu               sync.Mutex
}

func obtenerYFiltrarTorneosDesdeLCP(ctx context.Context, clientLCP pb.ContactarLCPClient, regionEntrenador string, nombreEntrenador string) ([]*pb.Torneo, error) {
	log.Printf("[%s] Consultando torneos disponibles a LCP...", nombreEntrenador)
	respuestaLCP, err := clientLCP.ConsultarTorneosDisponibles(ctx, &pb.TorneosRequest{
		SolicitanteInfo: fmt.Sprintf("Entrenador %s", nombreEntrenador),
	})
	if err != nil {
		return nil, fmt.Errorf("no se pudo obtener la lista de torneos de LCP: %w", err)
	}
	if respuestaLCP == nil || len(respuestaLCP.Torneos) == 0 {
		return nil, fmt.Errorf("LCP no reportó torneos disponibles en este momento")
	}
	var torneosEnRegion []*pb.Torneo
	for _, torneo := range respuestaLCP.Torneos {
		if torneo.Region == regionEntrenador {
			torneosEnRegion = append(torneosEnRegion, torneo)
		}
	}
	if len(torneosEnRegion) == 0 {
		return nil, fmt.Errorf("LCP no tiene torneos disponibles en tu región '%s'", regionEntrenador)
	}
	return torneosEnRegion, nil
}

func elegirTorneoManualmente(ctx context.Context, clientLCP pb.ContactarLCPClient, entrenador *Entrenador) (*pb.Torneo, error) {
	log.Printf("ENTRENADOR MANUAL [%s]: Buscando torneos en LCP para tu región %s...", entrenador.Name, entrenador.Region)
	torneosEnRegion, err := obtenerYFiltrarTorneosDesdeLCP(ctx, clientLCP, entrenador.Region, entrenador.Name)
	if err != nil {
		return nil, err
	}
	fmt.Printf("\n--- Entrenador %s, elige un torneo en %s (de LCP) ---\n", entrenador.Name, entrenador.Region)
	for i, torneo := range torneosEnRegion {
		fmt.Printf("%d. Torneo ID: %s (Región: %s)\n", i+1, torneo.Id, torneo.Region)
	}
	fmt.Printf("0. No inscribirse / Saltar\n")
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Ingresa el número del torneo de tu elección: ")
		input, _ := reader.ReadString('\n')
		choice, err := strconv.Atoi(strings.TrimSpace(input))
		if err != nil {
			fmt.Println("Entrada inválida. Por favor, ingresa un número.")
			continue
		}
		if choice == 0 {
			return nil, fmt.Errorf("decidiste no inscribirte manualmente en este momento")
		}
		if choice > 0 && choice <= len(torneosEnRegion) {
			return torneosEnRegion[choice-1], nil
		}
		fmt.Printf("Opción inválida. Elige un número entre 1 y %d, o 0 para saltar.\n", len(torneosEnRegion))
	}
}

func elegirTorneoAutomaticamente(ctx context.Context, clientLCP pb.ContactarLCPClient, entrenador *Entrenador) (*pb.Torneo, error) {
	log.Printf("ENTRENADOR AUTO [%s]: Buscando torneos en LCP para la región %s...", entrenador.Name, entrenador.Region)
	torneosEnRegion, err := obtenerYFiltrarTorneosDesdeLCP(ctx, clientLCP, entrenador.Region, entrenador.Name)
	if err != nil {
		return nil, err
	}
	localRand := rand.New(rand.NewSource(time.Now().UnixNano() + int64(os.Getpid())))
	indiceTorneo := localRand.Intn(len(torneosEnRegion))
	return torneosEnRegion[indiceTorneo], nil
}

func intentarInscripcionEntrenador(ctx context.Context, clientLCP pb.ContactarLCPClient, entrenador *Entrenador) {
	entrenador.mu.Lock()
	currentSuspension := entrenador.Suspension
	entrenador.mu.Unlock()

	log.Printf("[%s] Iniciando intento de inscripción (Manual: %t, Suspensión actual: %d días, Estado Torneo: %s)",
		entrenador.Name, entrenador.EsManual, currentSuspension, entrenador.TournamentStatus)

	if entrenador.TournamentStatus == "InscritoEnLCP" {
		log.Printf("[%s] ya tiene estado 'InscritoEnLCP' para torneo %s. No se reintenta.", entrenador.Name, entrenador.IdTournament)
		return
	}

	var torneoEscogido *pb.Torneo
	var err error

	if entrenador.EsManual {
		torneoEscogido, err = elegirTorneoManualmente(ctx, clientLCP, entrenador)
	} else {
		torneoEscogido, err = elegirTorneoAutomaticamente(ctx, clientLCP, entrenador)
	}

	if err != nil {
		log.Printf("[%s] Error al elegir torneo: %v", entrenador.Name, err)
		entrenador.TournamentStatus = "FalloEleccionTorneo"
		return
	}
	if torneoEscogido == nil {
		log.Printf("[%s] no seleccionó un torneo o no hay disponibles en su región.", entrenador.Name)
		entrenador.TournamentStatus = "NoInscrito (Sin selección/Disponibilidad)"
		return
	}

	log.Printf("[%s] ha escogido el torneo ID: %s (Región: %s). Intentando inscripción en LCP...", entrenador.Name, torneoEscogido.Id, torneoEscogido.Region)
	entrenador.TournamentStatus = "PendienteInscripcionLCP"

	reqInscripcion := &pb.InscripcionRequest{
		TorneoId:                 torneoEscogido.Id,
		EntrenadorId:             entrenador.ID,
		EntrenadorNombre:         entrenador.Name,
		EntrenadorRegion:         entrenador.Region,
		EntrenadorSuspensionDias: int32(currentSuspension),
	}

	respInscripcion, err := clientLCP.InscribirTorneo(ctx, reqInscripcion)
	if err != nil {
		log.Printf("[%s] Error gRPC al intentar inscribirse en el torneo %s: %v", entrenador.Name, torneoEscogido.Id, err)
		entrenador.TournamentStatus = "RechazadoPorLCP (Error gRPC)"
		entrenador.IdTournament = ""
		return
	}

	if respInscripcion.GetExito() {
		entrenador.TournamentStatus = "InscritoEnLCP"
		entrenador.IdTournament = respInscripcion.GetTorneoIdConfirmado()
		log.Printf("[%s] ¡INSCRIPCIÓN EXITOSA en LCP! Torneo: %s. Mensaje LCP: %s", entrenador.Name, entrenador.IdTournament, respInscripcion.GetMensaje())
	} else {
		log.Printf("[%s] LCP RECHAZÓ la inscripción en el torneo %s. Mensaje LCP: '%s' (Razón: %s)",
			entrenador.Name, torneoEscogido.Id, respInscripcion.GetMensaje(), respInscripcion.GetRazonRechazo())
		entrenador.TournamentStatus = "RechazadoPorLCP"
		entrenador.IdTournament = ""

		if respInscripcion.GetRazonRechazo() == pb.RazonRechazoInscripcion_RECHAZO_POR_SUSPENSION {
			entrenador.mu.Lock()
			if entrenador.Suspension > 0 {
				entrenador.Suspension--
				log.Printf("[%s] Suspensión reducida a %d días debido a intento fallido por suspensión.", entrenador.Name, entrenador.Suspension)
			}
			entrenador.mu.Unlock()
		}
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	lcpAddress := "localhost:50051"
	connLCP, err := grpc.Dial(lcpAddress, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		log.Fatalf("CLIENTE: No se pudo conectar al servidor LCP en %s: %v", lcpAddress, err)
	}
	defer connLCP.Close()
	clientLCP := pb.NewContactarLCPClient(connLCP)
	log.Printf("CLIENTE: Conectado al servidor LCP en %s", lcpAddress)

	numEntrenadores := 10
	regionesEntrenador := []string{"Kanto", "Johto", "Hoenn", "Sinnoh"}
	entrenadores := make([]*Entrenador, numEntrenadores)

	for i := 0; i < numEntrenadores; i++ {
		suspensionInicial := 0
		if i == 1 || i == 4 || i == 7 {
			suspensionInicial = rand.Intn(3) + 1
		}
		entrenadores[i] = &Entrenador{
			ID:               fmt.Sprintf("E%03d", i+1),
			Name:             fmt.Sprintf("Entrenador%02d", i+1),
			Region:           regionesEntrenador[rand.Intn(len(regionesEntrenador))],
			Suspension:       suspensionInicial,
			TournamentStatus: "NoInscrito",
			EsManual:         (i == 0),
		}
		if entrenadores[i].EsManual {
			entrenadores[i].Name = "Ash Ketchum (Manual)"
			entrenadores[i].Region = "Kanto"
			entrenadores[i].Suspension = 0
		}
	}

	numCiclosDeSimulacion := 4
	for ciclo := 0; ciclo < numCiclosDeSimulacion; ciclo++ {
		log.Printf("\n------------------- INICIO CICLO DE INSCRIPCIÓN #%d -------------------\n", ciclo+1)
		var wg sync.WaitGroup
		ctxCiclo, cancelCiclo := context.WithTimeout(context.Background(), 2*time.Minute)

		if entrenadores[0].EsManual && ciclo == 0 {
			log.Printf("\n--- Esperando acción del entrenador manual: %s ---\n", entrenadores[0].Name)
			intentarInscripcionEntrenador(ctxCiclo, clientLCP, entrenadores[0])
			log.Printf("--- Acción del entrenador manual %s completada ---\n", entrenadores[0].Name)
		}

		for i := 0; i < len(entrenadores); i++ {
			if entrenadores[i].EsManual && ciclo > 0 && entrenadores[i].TournamentStatus != "InscritoEnLCP" {
				log.Printf("[%s] (Manual) intentando de nuevo automáticamente en ciclo %d.", entrenadores[i].Name, ciclo+1)
			} else if entrenadores[i].EsManual && ciclo > 0 {
				continue
			} else if entrenadores[i].EsManual && ciclo == 0 {
				continue
			}

			if entrenadores[i].TournamentStatus != "InscritoEnLCP" {
				wg.Add(1)
				go func(e *Entrenador, cicloActual int) {
					defer wg.Done()
					ctxLlamada, cancelLlamada := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancelLlamada()
					time.Sleep(time.Duration(rand.Intn(150)+50) * time.Millisecond)
					log.Printf("--- [%s] intentando en Ciclo %d ---", e.Name, cicloActual+1)
					intentarInscripcionEntrenador(ctxLlamada, clientLCP, e)
				}(entrenadores[i], ciclo)
			}
		}
		wg.Wait()
		cancelCiclo()

		log.Printf("\n------------------- ESTADO FINAL CICLO #%d -------------------", ciclo+1)
		todosElegiblesInscritos := true
		for _, e := range entrenadores {
			log.Printf("ID: %s, Nombre: %s, Región: %s, Suspensión: %d, Estado Torneo: %s (Torneo ID: %s)",
				e.ID, e.Name, e.Region, e.Suspension, e.TournamentStatus, e.IdTournament)
			if !e.EsManual && e.TournamentStatus != "InscritoEnLCP" && e.Suspension == 0 {
				todosElegiblesInscritos = false
			}
		}
		log.Printf("----------------------------------------------------------------------\n")

		if todosElegiblesInscritos && ciclo > 0 {
			log.Println("Todos los entrenadores automáticos elegibles se han inscrito. Finalizando simulación de ciclos.")
			break
		}

		if ciclo < numCiclosDeSimulacion-1 {
			log.Println("...Esperando 1 segundo para el próximo ciclo de intentos...")
			time.Sleep(1 * time.Second)
		}
	}

	log.Println("\n========= SIMULACIÓN DE INSCRIPCIÓN FINALIZADA =========")
	log.Println("Estado final de todos los entrenadores:")
	for _, e := range entrenadores {
		log.Printf("ID: %s, Nombre: %s, Suspensión: %d, Estado Torneo: %s (Torneo ID: %s)",
			e.ID, e.Name, e.Suspension, e.TournamentStatus, e.IdTournament)
	}
}