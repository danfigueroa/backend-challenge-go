SHELL := /bin/bash

GOLANGCI_LINT_VERSION := v2.13.2
GOLANGCI_LINT := CGO_ENABLED=0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

COMPOSE := docker compose
MIGRATE := $(COMPOSE) run --rm migrate

.DEFAULT_GOAL := help

.PHONY: help
help: ## List available targets
	@grep -E '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-22s\033[0m %s\n", $$1, $$2}'

## ---------- Build ----------

.PHONY: build
build: ## Build the service binary into ./bin
	CGO_ENABLED=0 go build -trimpath -o bin/wallet ./cmd/wallet

.PHONY: tidy
tidy: ## Tidy and verify Go modules
	go mod tidy
	go mod verify

## ---------- Quality ----------

.PHONY: fmt
fmt: ## Format code
	gofmt -s -w .

.PHONY: vet
vet: ## Run go vet (all build tags)
	go vet ./...
	go vet -tags=integration,e2e,faultinject ./...

.PHONY: lint
lint: vet ## Run gofmt check, go vet and golangci-lint
	@test -z "$$(gofmt -s -l . | tee /dev/stderr)" || (echo "gofmt found unformatted files" && exit 1)
	$(GOLANGCI_LINT) run ./...

## ---------- Tests ----------

.PHONY: test
test: ## Unit tests with race detector
	go test -race -count=1 ./...

.PHONY: test-cover
test-cover: ## Unit tests with coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: test-integration
test-integration: ## Integration tests against real containers (testcontainers-go)
	go test -race -count=1 -tags=integration -timeout=20m ./...

.PHONY: test-e2e
test-e2e: ## Multi-instance and fault-injection tests (requires `make up-e2e`)
	go test -count=1 -tags=e2e -timeout=30m ./test/e2e/...

.PHONY: fuzz
fuzz: ## Fuzz money parsing for 30s
	go test -run=^$$ -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money

## ---------- Environment ----------

.PHONY: up
up: ## Start the full stack
	$(COMPOSE) up --build -d

.PHONY: up-e2e
up-e2e: ## Start the stack with fault-injection binaries for e2e tests
	$(COMPOSE) -f docker-compose.yml -f docker-compose.e2e.yml up --build -d

.PHONY: down
down: ## Stop the stack and remove volumes
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Tail application logs
	$(COMPOSE) logs -f app-1 app-2 app-3

## ---------- Database ----------

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	$(MIGRATE) up

.PHONY: migrate-down
migrate-down: ## Revert the last migration (N=1 by default)
	$(MIGRATE) down $(or $(N),1)

.PHONY: migrate-version
migrate-version: ## Show current migration version
	$(MIGRATE) version

## ---------- Load ----------

.PHONY: load
load: ## Run k6 load test (dockerized k6)
	$(COMPOSE) --profile load run --rm k6
