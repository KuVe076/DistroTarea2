# DISTROTAREA2/LCP/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copiar archivos de módulos DESDE LA RAÍZ DEL CONTEXTO DE BUILD (DISTROTAREA2/)
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copiar el directorio proto DESDE LA RAÍZ DEL CONTEXTO DE BUILD
COPY proto ./proto

# Copiar el código fuente específico de LCP.
# Si el contexto de build es DISTROTAREA2/, entonces LCP/lcp.go es la ruta correcta.
COPY LCP/lcp.go ./lcp.go 
# (Asumo que renombraste lcp.go a lcp_server.go como en el código que me pasaste, o viceversa)
# Si el archivo sigue siendo LCP/lcp.go en tu disco:
# COPY LCP/lcp.go ./lcp.go

# Copiar el archivo JSON que LCP necesita leer, DESDE LA RAÍZ DEL CONTEXTO DE BUILD
# Ya que lcp_server.go lo busca como "entrenadores_pequeno.json" (relativo a su ejecución)
# y lo queremos disponible en el WORKDIR /app de esta etapa.
COPY Entrenador/entrenadores_pequeno.json ./entrenadores_pequeno.json

# Compilar la aplicación LCP.
# El comando 'go build' buscará el archivo .go especificado en el WORKDIR actual (/app)
# Ajusta el nombre del archivo si es lcp.go o lcp_server.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /lcp_app_built lcp.go
# o si es lcp.go:
# RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /lcp_app_built lcp.go

# Etapa 2: Imagen final ligera
FROM alpine:latest

# Establecer el directorio de trabajo en la imagen final
WORKDIR /app_distro

# Copiar el binario compilado desde la etapa de builder
COPY --from=builder /lcp_app_built .

# Copiar el archivo JSON necesario en runtime desde la etapa de builder
# Se copiará a /app_distro/entrenadores_pequeno.json
COPY --from=builder /app/entrenadores_pequeno.json .

# Puerto que expone el servicio gRPC de LCP
EXPOSE 50051

# Comando para ejecutar la aplicación LCP
# El path es relativo al WORKDIR (/app_distro)
CMD ["./lcp_app_built"]