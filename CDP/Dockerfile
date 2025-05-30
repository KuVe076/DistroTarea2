FROM golang:1.24-alpine AS builder


WORKDIR /app


COPY go.mod go.sum ./
RUN go mod download && go mod verify


COPY proto ./proto


COPY CDP/cdp.go ./cdp.go


RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /cdp_app_built cdp.go


FROM alpine:latest


WORKDIR /app_distro


COPY --from=builder /cdp_app_built .

CMD ["./cdp_app_built"]