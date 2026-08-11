# Atajos de desarrollo. Todo corre en contenedores: solo hace falta Docker.

GO_IMAGE    := golang:1.26-alpine
PWD_MOUNT   := $(CURDIR):/src
GOMOD_CACHE := sv-media-gomod:/go/pkg/mod

DOCKER_GO := docker run --rm -v "$(PWD_MOUNT)" -v "$(GOMOD_CACHE)" -w /src $(GO_IMAGE)

.PHONY: help
help: ## Muestra esta ayuda
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: tidy
tidy: ## Ordena go.mod / go.sum
	$(DOCKER_GO) go mod tidy

.PHONY: build
build: ## Compila todos los paquetes
	$(DOCKER_GO) go build ./...

.PHONY: vet
vet: ## Corre go vet
	$(DOCKER_GO) go vet ./...

.PHONY: fmt
fmt: ## Formatea el codigo
	$(DOCKER_GO) gofmt -l -w .

.PHONY: test
test: ## Corre la suite (no necesita servicios levantados: core se simula)
	$(DOCKER_GO) go test ./...

.PHONY: up
up: ## Levanta LiveKit + signaling (requiere el stack de core levantado)
	docker compose up -d --build

.PHONY: up-proxy
up-proxy: ## Levanta todo incluyendo Caddy con TLS (requiere DNS configurado)
	docker compose --profile proxy up -d --build

.PHONY: down
down: ## Baja el stack
	docker compose down

.PHONY: logs
logs: ## Sigue los logs de signaling
	docker compose logs -f signaling

.PHONY: logs-livekit
logs-livekit: ## Sigue los logs del SFU
	docker compose logs -f livekit
