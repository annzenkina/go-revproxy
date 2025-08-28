APP := reverse-proxy
COMPOSE_FILE := docker-compose.yaml


.PHONY: up down logs ps build run tidy clean

up:
	podman machine start || true
	podman compose -f $(COMPOSE_FILE) up -d

down:
	podman compose -f $(COMPOSE_FILE) down

logs:
	podman compose -f $(COMPOSE_FILE) logs -f

ps:
	podman ps --format "table {{.Names}}\t{{.Image}}\t{{.Ports}}"

build:
	go build -o bin/$(APP) ./main.go
run:
	go run ./main.go --config config.yaml

tidy:
	go mod tidy

clean:
	rm -rf bin

:
	curl -s localhost:9001 | head -n1

call-server2:
	curl -s localhost:9002 | head -n1

call-server3:
	curl -s localhost:9003 | head -n1
