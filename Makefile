SHELL := /bin/bash

ifeq ($(shell uname -s),Darwin)
export CGO_ENABLED ?= 0
endif

GOLANGCI_LINT_VERSION := v2.13.2
GOLANGCI_LINT := go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

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
	$(GOLANGCI_LINT) run --build-tags=integration,e2e,faultinject ./...

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
test-e2e: ## Multi-process and fault-injection tests (requires `make up-infra`; `make up` also enables the compose smoke test)
	go test -race -count=1 -tags=e2e -timeout=30m -v ./test/e2e/...

.PHONY: fuzz
fuzz: ## Fuzz money parsing for 30s
	go test -run=^$$ -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money

## ---------- Environment ----------

.PHONY: up
up: ## Start the full stack (3 instances + observability) and wait until healthy
	$(COMPOSE) up --build -d --wait

.PHONY: up-infra
up-infra: ## Start only PostgreSQL, Keycloak and LocalStack (used by e2e tests)
	$(COMPOSE) up -d --wait postgres keycloak localstack

.PHONY: down
down: ## Stop the stack and remove volumes
	$(COMPOSE) down -v --remove-orphans

.PHONY: logs
logs: ## Tail application logs
	$(COMPOSE) logs -f app-1 app-2 app-3

## ---------- Database ----------

.PHONY: migrate-up
migrate-up: ## Apply all pending migrations
	$(MIGRATE) migrate up

.PHONY: migrate-down
migrate-down: ## Revert the last migration (N=1 by default)
	$(MIGRATE) migrate down $(or $(N),1)

.PHONY: migrate-version
migrate-version: ## Show current migration version
	$(MIGRATE) migrate version

## ---------- Load ----------

.PHONY: load
load: ## Baseline k6 load test against the running stack (see docs/LOAD_TEST.md)
	$(COMPOSE) --profile load run --rm k6

.PHONY: load-stress
load-stress: ## k6 stress profile beyond the local capacity
	LOAD_RATE=200 LOAD_HOT_VUS=40 $(COMPOSE) --profile load run --rm k6
