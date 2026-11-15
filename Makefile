COMPOSE ?= docker compose
BIN     ?= bin/agent-otel-kit
OTELCOL_IMAGE := otel/opentelemetry-collector-contrib:0.164.0

.PHONY: help up up-perses up-debug down clean demo doctor screenshots build test test-collector lint dashboards validate

help: ## List targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-15s %s\n", $$1, $$2}'

up: ## Start collector, Prometheus, Loki, Tempo and Grafana
	$(COMPOSE) up -d
	@echo "Grafana: http://localhost:3000   OTLP: http://localhost:4318 (HTTP), localhost:4317 (gRPC)"

up-perses: ## Same as up, plus Perses on http://localhost:8080
	$(COMPOSE) --profile perses up -d

up-debug: ## Start with the collector debug exporter turned on
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml up -d

down: ## Stop the stack (data volumes are kept)
	$(COMPOSE) --profile perses down

clean: ## Stop the stack and delete its data volumes
	$(COMPOSE) --profile perses down -v

demo: ## Run two short claude -p sessions that send telemetry to the stack
	scripts/demo.sh

doctor: build ## Check the stack and agent configs
	$(BIN) doctor

screenshots: ## Capture the dashboards into docs/screenshots with headless Chrome
	scripts/screenshots.sh docs/screenshots

build: ## Build the CLI into bin/
	go build -o $(BIN) ./cmd/agent-otel-kit

test: ## Run Go tests
	go test ./...

test-collector: ## Run the collector config against recorded agent payloads (needs docker)
	scripts/test-collector.sh

lint: ## gofmt and go vet
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "$$out"; exit 1; fi
	go vet ./...

dashboards: ## Regenerate Grafana and Perses dashboards from tools/gendashboards
	go run ./tools/gendashboards

validate: ## Validate compose and collector configs
	$(COMPOSE) config -q
	$(COMPOSE) -f compose.yaml -f compose.debug.yaml config -q
	docker run --rm -v "$(CURDIR)/collector:/c:ro" $(OTELCOL_IMAGE) validate --config=/c/config.yaml
	docker run --rm -v "$(CURDIR)/collector:/c:ro" $(OTELCOL_IMAGE) validate --config=/c/config.yaml --config=/c/debug.yaml
