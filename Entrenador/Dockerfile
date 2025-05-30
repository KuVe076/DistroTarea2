FROM golang:1.24-alpine AS builder
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY proto ./proto
COPY Entrenador/entrenador.go . 
COPY Entrenador/entrenadores_pequeno.json ./entrenadores_pequeno.json

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /entrenadores_app entrenador.go


FROM alpine:latest
COPY --from=builder /entrenadores_app /entrenadores_app

COPY --from=builder /app/entrenadores_pequeno.json /entrenadores_pequeno.json 


CMD ["/entrenadores_app"]