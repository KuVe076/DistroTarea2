package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	pb "DISTROTAREA2/proto"
	"google.golang.org/grpc"
)

const (
	port = ":50051"
)

type lcpServer struct {
	pb.UnimplementedContactarLCPServer
	mu            sync.Mutex
	torneos       map[string]*pb.Torneo
	inscripciones map[string]string
}

func nuevoLCPServer() *lcpServer {
	s := &lcpServer{
		torneos:       make(map[string]*pb.Torneo),
		inscripciones: make(map[string]string),
	}
	s.inicializarTorneosDeEjemplo()
	return s
}

func (s *lcpServer) inicializarTorneosDeEjemplo() {
	s.mu.Lock()
	defer s.mu.Unlock()
	regionesDisponibles := []string{"Kanto", "Johto", "Hoenn", "Sinnoh", "Unova"}
	numTorneos := 10
	for i := 0; i < numTorneos; i++ {
		torneoID := fmt.Sprintf("T%03d", i+1)
		region := regionesDisponibles[rand.Intn(len(regionesDisponibles))]
		s.torneos[torneoID] = &pb.Torneo{
			Id:     torneoID,
			Region: region,
		}
	}
	log.Printf("LCP: %d torneos de ejemplo inicializados.", len(s.torneos))
}

func (s *lcpServer) ConsultarTorneosDisponibles(ctx context.Context, req *pb.TorneosRequest) (*pb.TorneosResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	log.Printf("LCP: Recibida solicitud de lista de torneos de '%s'", req.GetSolicitanteInfo())
	listaDeTorneos := make([]*pb.Torneo, 0, len(s.torneos))
	for _, torneo := range s.torneos {
		listaDeTorneos = append(listaDeTorneos, torneo)
	}
	return &pb.TorneosResponse{Torneos: listaDeTorneos}, nil
}

func (s *lcpServer) InscribirTorneo(ctx context.Context, req *pb.InscripcionRequest) (*pb.InscripcionResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entrenadorID := req.GetEntrenadorId()
	torneoIDDeseado := req.GetTorneoId()
	nombreEntrenador := req.GetEntrenadorNombre()
	diasSuspensionEntrenador := req.GetEntrenadorSuspensionDias()

	log.Printf("LCP: Intento de inscripción: Entrenador '%s' (ID: %s, Suspensión: %d días) al Torneo ID: %s",
		nombreEntrenador, entrenadorID, diasSuspensionEntrenador, torneoIDDeseado)

	if diasSuspensionEntrenador > 0 {
		mensaje := fmt.Sprintf("Estás suspendido por %d días y no puedes inscribirte.", diasSuspensionEntrenador)
		log.Printf("LCP: RECHAZADO (Suspensión) - Entrenador '%s'. %s", nombreEntrenador, mensaje)
		return &pb.InscripcionResponse{
			Exito:        false,
			Mensaje:      mensaje,
			RazonRechazo: pb.RazonRechazoInscripcion_RECHAZO_POR_SUSPENSION,
		}, nil
	}

	torneoInscritoID, estaInscrito := s.inscripciones[entrenadorID]
	if estaInscrito {
		if torneoInscritoID == torneoIDDeseado {
			mensaje := "Ya estabas inscrito en este torneo."
			log.Printf("LCP: ACEPTADO (Ya inscrito en este torneo) - Entrenador '%s' en Torneo ID: %s.", nombreEntrenador, torneoIDDeseado)
			return &pb.InscripcionResponse{Exito: true, Mensaje: mensaje, TorneoIdConfirmado: torneoIDDeseado}, nil
		}
		mensaje := fmt.Sprintf("Ya estás inscrito en otro torneo (ID: %s). No puedes inscribirte en %s.", torneoInscritoID, torneoIDDeseado)
		log.Printf("LCP: RECHAZADO (Inscrito en otro torneo) - Entrenador '%s'. %s", nombreEntrenador, mensaje)
		return &pb.InscripcionResponse{
			Exito:        false,
			Mensaje:      mensaje,
			RazonRechazo: pb.RazonRechazoInscripcion_RECHAZO_YA_INSCRITO_OTRO_TORNEO,
		}, nil
	}

	torneoSeleccionado, existeTorneo := s.torneos[torneoIDDeseado]
	if !existeTorneo {
		mensaje := fmt.Sprintf("El torneo con ID '%s' no existe.", torneoIDDeseado)
		log.Printf("LCP: RECHAZADO (Torneo no existe) - Entrenador '%s'. %s", nombreEntrenador, mensaje)
		return &pb.InscripcionResponse{
			Exito:        false,
			Mensaje:      mensaje,
			RazonRechazo: pb.RazonRechazoInscripcion_RECHAZO_TORNEO_NO_EXISTE,
		}, nil
	}

	// CAMBIO: Eliminada la sección de validación de capacidad del torneo.

	s.inscripciones[entrenadorID] = torneoIDDeseado
	mensajeExito := fmt.Sprintf("¡Inscripción exitosa al torneo ID: %s (Región: %s)!", torneoSeleccionado.Id, torneoSeleccionado.Region)
	log.Printf("LCP: ÉXITO - Entrenador '%s' (ID: %s) inscrito en Torneo ID: %s.", nombreEntrenador, entrenadorID, torneoIDDeseado)

	return &pb.InscripcionResponse{
		Exito:             true,
		Mensaje:           mensajeExito,
		TorneoIdConfirmado: torneoIDDeseado,
	}, nil
}

func main() {
	rand.Seed(time.Now().UnixNano())
	lis, err := net.Listen("tcp", port)
	if err != nil {
		log.Fatalf("LCP: Falló al escuchar en el puerto %s: %v", port, err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterContactarLCPServer(grpcServer, nuevoLCPServer())
	log.Printf("LCP: Servidor gRPC escuchando en %s", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("LCP: Falló al servir gRPC: %v", err)
	}
}