# backend-challenge-go

Teste técnico para Jungle Gaming — serviço em Go para **processamento distribuído de operações de apostas** sobre carteiras de jogadores, com garantias de integridade financeira, idempotência persistente, concorrência entre múltiplas instâncias e recuperação de falhas.

> 🚧 Em desenvolvimento. O enunciado está em [`docs/CHALLENGE.md`](docs/CHALLENGE.md), as decisões técnicas em [`ARCHITECTURE.md`](ARCHITECTURE.md) e o progresso por requisito em [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md).

## Stack

| Responsabilidade | Tecnologia |
|---|---|
| Linguagem | Go 1.27.1 |
| Composição | Uber Fx |
| HTTP | `net/http` |
| Persistência | PostgreSQL + `pgx/v5` + `golang-migrate` |
| Autenticação | Keycloak (OAuth 2.0 / OIDC, `client_credentials`) |
| Mensageria | AWS SQS/SNS FIFO via LocalStack 4.14.0 |
| Observabilidade | `log/slog`, Prometheus, OpenTelemetry, Jaeger, Grafana |
| Testes | `go test -race`, testcontainers-go, k6 |

## Pré-requisitos

- Go 1.27.1+
- Docker 27+ e Docker Compose v2
- GNU Make

## Comandos

```sh
make help               # lista todos os alvos
make test               # testes unitários com -race
make test-cover         # testes unitários com relatório de cobertura
make fuzz               # fuzzing do parser de Money por 30s
make lint               # gofmt, go vet e golangci-lint
```

## Testes

```sh
make test               # unitários (go test -race ./...)
make test-integration   # unitários + integração com PostgreSQL real (testcontainers-go)
```

Os testes de integração usam a build tag `integration` e precisam apenas do Docker em execução: o container `postgres:18.6-alpine3.24` é criado e removido automaticamente. Comando equivalente sem Make:

```sh
go test -race -count=1 -tags=integration ./...
```

> **macOS**: o detector de corrida não precisa de cgo no macOS. Se o linker falhar com `unknown architecture arm64e` (Command Line Tools desatualizadas em relação ao SDK), execute com `CGO_ENABLED=0`; o `Makefile` já faz isso automaticamente no Darwin.

## Executando o binário

```sh
make build                                   # gera ./bin/wallet
./bin/wallet help

export DATABASE_MIGRATIONS_URL=postgres://wallet_owner:...@localhost:5432/wallet
./bin/wallet migrate up                      # aplica as migrations
./bin/wallet migrate down 1                  # reverte a última migration
./bin/wallet migrate version                 # versão atual

export DATABASE_URL=postgres://wallet_service:...@localhost:5432/wallet
./bin/wallet serve                           # sobe o serviço (SIGTERM encerra graciosamente)
```

Imagem de container:

```sh
docker build -t wallet-service:dev .
docker run --rm wallet-service:dev help
```

Endpoints administrativos (`ADMIN_ADDR`, padrão `:9090`): `GET /metrics`, `GET /health/live`, `GET /health/ready`.

## Variáveis de ambiente

Valores de exemplo em [`.env.example`](.env.example). A configuração é validada inteira na inicialização e todos os problemas são reportados de uma vez.

| Variável | Padrão | Descrição |
|---|---|---|
| `APP_ROLES` | `api,consumer,outbox,pendingref` | Papéis executados pela instância |
| `APP_INSTANCE_ID` | hostname | Identificador da instância (logs, locks de outbox) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `APP_START_TIMEOUT` / `APP_SHUTDOWN_TIMEOUT` | `60s` / `30s` | Prazos de inicialização e encerramento |
| `HTTP_ADDR` / `ADMIN_ADDR` | `:8080` / `:9090` | API pública e servidor administrativo |
| `HTTP_*_TIMEOUT`, `HTTP_MAX_BODY_BYTES` | ver `.env.example` | Limites do servidor HTTP |
| `DATABASE_URL` | — (obrigatório) | Conexão do serviço (`wallet_service`) |
| `DATABASE_MIGRATIONS_URL` | `DATABASE_URL` | Conexão do dono do schema para migrations |
| `DATABASE_MAX_CONNS` / `DATABASE_MIN_CONNS` | `20` / `2` | Tamanho do pool |
| `DATABASE_LOCK_TIMEOUT` / `DATABASE_STATEMENT_TIMEOUT` | `2s` / `5s` | Timeouts por transação |
| `RETRY_ATTEMPTS` / `RETRY_BASE_DELAY` / `RETRY_MAX_DELAY` | `5` / `20ms` / `500ms` | Retry de falhas transitórias |
| `PENDING_TTL` | `30m` | Prazo para uma referência chegar |
| `PENDING_BASE_DELAY` / `PENDING_MAX_DELAY` | `1s` / `60s` | Backoff exponencial das pendências |
| `PENDING_CLAIM_LEASE` / `PENDING_POLL_INTERVAL` / `PENDING_BATCH_SIZE` | `30s` / `1s` / `50` | Worker de pendências |
| `OTEL_TRACES_ENABLED` / `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_TRACES_SAMPLE_PERCENT` | `false` / — / `100` | Tracing OpenTelemetry |

As seções de Docker Compose, filas, autenticação, exemplos de chamadas e testes multi-instância/falhas serão adicionadas à medida que cada componente for entregue.
