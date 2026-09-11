SHELL := /bin/bash
BIN   := bin
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: help build test smoke fmt vet demo demo-stop stack stack-build stack-stop stack-logs submit clean

help:
	@echo "build        compile dtp, dtp-master, dtp-runner into ./$(BIN)"
	@echo "test         run unit tests"
	@echo "demo         local backend: MinIO in docker + master on the host (no Nomad)"
	@echo "demo-stop    tear the local demo down"
	@echo "stack        full stack: Nomad cluster + MinIO + master in docker compose"
	@echo "stack-stop   tear the full stack down"
	@echo "submit       submit examples/regression.json and follow it"
	@echo "smoke        end-to-end assertions against a running master"

build:
	@mkdir -p $(BIN)
	go build -trimpath -o $(BIN)/ ./cmd/...
	@echo "built: $$(ls $(BIN) | tr '\n' ' ')"

test:
	go test ./...

smoke: build
	./scripts/smoke.sh

fmt:
	gofmt -l -w cmd internal

vet:
	go vet ./...

demo: build
	./scripts/demo-local.sh

demo-stop:
	-@kill $$(cat .dtp/master.pid 2>/dev/null) 2>/dev/null || true
	-@docker rm -f dtp-minio >/dev/null 2>&1 || true
	@echo "local demo stopped"

stack:
	$(COMPOSE) up -d --build
	@echo
	@echo "dashboard  http://localhost:8080"
	@echo "nomad ui   http://localhost:4646"
	@echo "minio      http://localhost:9001  (dtpadmin / dtpadmin123)"

stack-build:
	$(COMPOSE) build

stack-stop:
	$(COMPOSE) down -v

stack-logs:
	$(COMPOSE) logs -f master

submit: build
	$(BIN)/dtp submit examples/regression.json -w

clean: demo-stop
	rm -rf $(BIN) .dtp
