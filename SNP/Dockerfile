# DISTROTAREA2/SNP/Dockerfile

# Etapa 1: Compilación
FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY proto ./proto
COPY SNP/snp.go . 

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /snp_app snp.go

# Etapa 2: Imagen final ligera
FROM alpine:latest
COPY --from=builder /snp_app /snp_app
CMD ["/snp_app"]