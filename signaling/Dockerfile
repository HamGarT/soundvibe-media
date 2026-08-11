# Build multi-stage con imagen final alpine. Mismo patron que
# soundvibe-core/Dockerfile, para que los dos servicios Go del proyecto se
# construyan y se operen igual.
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Las dependencias primero: la capa de modulos se reutiliza mientras no cambien
# go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 para un binario estatico que corra en alpine sin glibc.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/signaling ./signaling/cmd/signaling

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S soundvibe \
    && adduser -S -G soundvibe soundvibe

COPY --from=builder /out/signaling /usr/local/bin/signaling

USER soundvibe

EXPOSE 8092

# Se usa GET y no --spider: --spider manda HEAD, y el router responde 405 a HEAD
# en una ruta registrada como GET, con lo que el contenedor queda marcado
# unhealthy aunque el servicio funcione perfectamente.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget --quiet --tries=1 --output-document=/dev/null http://localhost:8092/health || exit 1

ENTRYPOINT ["/usr/local/bin/signaling"]
