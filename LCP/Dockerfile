# DISTROTAREA2/LCP/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder

# Establecer el directorio de trabajo dentro del contenedor
WORKDIR /app

# Copiar archivos de módulos desde la raíz del contexto de build
# El contexto de build es DISTROTAREA2/ (definido en docker-compose.yml)
COPY go.mod go.sum ./
RUN go mod download && go mod verify

# Copiar el directorio proto desde la raíz del contexto de build
COPY proto ./proto

# Copiar el código fuente específico de LCP desde su subdirectorio (relativo a la raíz del contexto)
COPY LCP/lcp.go .
# Ahora /app/lcp.go existe

# Compilar la aplicación LCP. El comando 'go build' buscará 'lcp.go' en el WORKDIR actual (/app)
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /lcp_server lcp.go

# Etapa 2: Imagen final ligera
FROM alpine:latest

# Copiar el binario compilado desde la etapa de builder
COPY --from=builder /lcp_server /lcp_server

# Si LCP carga 'entrenadores_pequeno.json' directamente desde la raíz del proyecto
# (Asegúrate que la ruta en lcp.go sea "/entrenadores_pequeno.json" si lo haces así)
# COPY --from=builder /app/entrenadores_pequeno.json /entrenadores_pequeno.json 
# Sin embargo, si lcp.go lee desde el contexto, y el contexto es la raíz del proyecto, entonces
# el archivo JSON no estaría dentro de /app en la etapa de builder a menos que lo copies explícitamente allí.
# Si lcp_server.go espera "entrenadores_pequeno.json" en el mismo dir que el binario, necesitarías:
# COPY entrenadores_pequeno.json . # en la etapa de builder, si es relativo a LCP/ y no a la raíz del proyecto
# O si está en la raíz del proyecto y el binario está en /:
# COPY --from=builder /app/entrenadores_pequeno.json /entrenadores_pequeno.json # (si se copió a /app en builder)
# O directamente desde el contexto si el Dockerfile de LCP tiene acceso a él:
# COPY ../entrenadores_pequeno.json /entrenadores_pequeno.json # (esto es si el contexto de lcp Dockerfile fuera LCP/)
# Con el contexto en la raíz para docker-compose, es más simple:
# En la etapa de builder, si lcp.go lo necesita para compilar (no es el caso):
# COPY entrenadores_pequeno.json .
# En la etapa final, si lcp_server (el binario) lo necesita en runtime:
# COPY entrenadores_pequeno.json / # O donde lo espere el binario

# Puerto que expone el servicio gRPC de LCP
EXPOSE 50051

# Comando para ejecutar la aplicación LCP
CMD ["/lcp_server"]