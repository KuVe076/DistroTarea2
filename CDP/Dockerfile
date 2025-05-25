# DISTROTAREA2/CDP/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder

# Establecer el directorio de trabajo dentro del contenedor de build
WORKDIR /app

# Copiar archivos de módulos DESDE LA RAÍZ DEL CONTEXTO DE BUILD (DISTROTAREA2/)
# (Asumiendo que el contexto de build para este Dockerfile cuando se usa con
# docker-compose será la raíz del proyecto DISTROTAREA2/)
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copiar el directorio proto DESDE LA RAÍZ DEL CONTEXTO DE BUILD
# CDP no usa directamente gRPC para recibir, pero podría necesitar los tipos definidos
# si interactúa con LCP vía gRPC para validar entrenadores (como sugiere el PDF).
# Si no usa directamente el proto, esta línea podría omitirse, pero no daña.
COPY proto ./proto

# Copiar el código fuente específico del CDP.
# Si el contexto de build es DISTROTAREA2/, entonces CDP/cdp.go es la ruta correcta.
COPY CDP/cdp.go ./cdp.go

# Compilar la aplicación CDP.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /cdp_app_built cdp.go

# Etapa 2: Imagen final ligera
FROM alpine:latest

# Establecer el directorio de trabajo en la imagen final
WORKDIR /app_distro

# Copiar SOLO el binario compilado desde la etapa de builder
COPY --from=builder /cdp_app_built .

# CDP se conecta a RabbitMQ. No expone puertos directamente para ser llamado,
# sino que consume de RabbitMQ.

# Comando para ejecutar la aplicación CDP
CMD ["./cdp_app_built"]