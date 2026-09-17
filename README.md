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
| `AUTH_ISSUER` | — (obrigatório com papel `api`) | Valor esperado do `iss` dos tokens |
| `AUTH_JWKS_URL` | — (obrigatório com papel `api`) | Endpoint de chaves do IdP (pode usar a rede interna) |
| `AUTH_AUDIENCE` | `wallet-api` | Audience exigida |

## Autenticação

O Keycloak importa automaticamente o realm `wagering` (`deploy/keycloak/realm-wagering.json`) com clients `client_credentials` de teste. Os segredos são apenas para uso local:

| Client | Segredo | Uso |
|---|---|---|
| `provider-a` / `provider-b` | `provider-a-local-secret` / `provider-b-local-secret` | Provedores de jogos |
| `wallet-internal` | `wallet-internal-local-secret` | Operações de carteira e reconciliação |
| `provider-a-short-lived` | `provider-a-short-lived-local-secret` | Token de 2 s |
| `provider-unprivileged` | `provider-unprivileged-local-secret` | Sem permissões |
| `foreign-audience` | `foreign-audience-local-secret` | Token sem audience da API |

Obter tokens (ajuste o host/porta do Keycloak):

```sh
KC=http://localhost:8081/realms/wagering/protocol/openid-connect/token
token() { curl -s -X POST "$KC" -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-local-secret" | jq -r .access_token; }
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
```

## Exemplos de chamadas

```sh
API=http://localhost:8080

curl -s -X POST $API/wallets -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'

WALLET_ID=...   # id retornado acima

curl -s -X POST $API/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"'$WALLET_ID'","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'

curl -s $API/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER_A"
curl -s "$API/wallets/$WALLET_ID/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
curl -s -X POST $API/wallets/$WALLET_ID/reconciliation -H "Authorization: Bearer $INTERNAL"
```

Status e corpos de cada situação (200, 202, 400, 401, 403, 404, 409, 413, 415, 422, 503) estão em [ARCHITECTURE.md › Contrato HTTP](ARCHITECTURE.md#contrato-http).

Os testes de integração da API (`internal/adapter/httpapi`) sobem **Keycloak e PostgreSQL reais** via testcontainers.

As seções de Docker Compose, filas e testes multi-instância/falhas serão adicionadas à medida que cada componente for entregue.
