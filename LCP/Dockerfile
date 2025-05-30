
FROM golang:1.24-alpine AS builder

WORKDIR /app


COPY go.mod go.sum ./
RUN go mod download && go mod verify


COPY proto ./proto


COPY LCP/lcp.go ./lcp.go 

COPY Entrenador/entrenadores_pequeno.json ./entrenadores_pequeno.json


RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /lcp_app_built lcp.go


FROM alpine:latest


WORKDIR /app_distro


COPY --from=builder /lcp_app_built .


COPY --from=builder /app/entrenadores_pequeno.json .


EXPOSE 50051

CMD ["./lcp_app_built"]