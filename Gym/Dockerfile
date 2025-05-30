FROM golang:1.24-alpine AS builder


WORKDIR /app


COPY go.mod go.sum ./
RUN go mod download && go mod verify


COPY proto ./proto


COPY Gym/gym.go ./gym.go


RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /gym_app_built gym.go


FROM alpine:latest


WORKDIR /app_distro


COPY --from=builder /gym_app_built .




CMD ["./gym_app_built"]