# DISTROTAREA2/Gym/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder

# Establecer el directorio de trabajo dentro del contenedor de build
WORKDIR /app

# Copiar archivos de módulos DESDE LA RAÍZ DEL CONTEXTO DE BUILD (DISTROTAREA2/)
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copiar el directorio proto DESDE LA RAÍZ DEL CONTEXTO DE BUILD
COPY proto ./proto

# Copiar el código fuente específico del Gimnasio.
# Si el contexto de build es DISTROTAREA2/, entonces Gym/gym.go es la ruta correcta.
COPY Gym/gym.go ./gym.go

# Compilar la aplicación Gimnasio.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /gym_app_built gym.go

# Etapa 2: Imagen final ligera
FROM alpine:latest

# Establecer el directorio de trabajo en la imagen final
WORKDIR /app_distro

# Copiar SOLO el binario compilado desde la etapa de builder
COPY --from=builder /gym_app_built .

# Los puertos específicos (50052-50060) se mapearán en docker-compose.yml
# No es estrictamente necesario EXPOSE aquí si se hace en compose,
# pero puede ser informativo.
# EXPOSE 50052
# EXPOSE 50053
# ... (hasta el último puerto de gimnasio)

# Comando para ejecutar la aplicación Gimnasio (que inicia todos los servidores de gimnasio)
CMD ["./gym_app_built"]