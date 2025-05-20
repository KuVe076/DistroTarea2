# DISTROTAREA2/Entrenador/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY proto ./proto
COPY Entrenador/entrenador.go . 
COPY entrenadores_pequeno.json . 

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /entrenadores_app entrenador.go

# Etapa 2: Imagen final ligera
FROM alpine:latest
COPY --from=builder /entrenadores_app /entrenadores_app
# Copiar el archivo JSON que el cliente necesita leer en runtime.
# El binario /entrenadores_app lo buscará en la ruta especificada en el código Go.
COPY --from=builder /app/entrenadores_pequeno.json /entrenadores_pequeno.json 
# En entrenador.go, la ruta debe ser: const jsonEntrenadoresFile = "/entrenadores_pequeno.json"

CMD ["/entrenadores_app"]