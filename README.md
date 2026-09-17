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
make up                 # stack completa via Docker Compose (3 instâncias)
make up-infra           # apenas PostgreSQL, Keycloak e LocalStack
make down               # derruba a stack e remove volumes
make load               # teste de carga k6 (perfil base)
make load-stress        # teste de carga k6 (perfil de estresse)
```

## Testes

```sh
make test               # unitários (go test -race ./...)
make test-integration   # unitários + integração com PostgreSQL, Keycloak e LocalStack reais (testcontainers-go)
make test-e2e           # processos independentes, kill -9 e injeção de falhas (requer make up-infra)
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

export DATABASE_MIGRATIONS_URL=postgres://wallet_owner:...@localhost:55432/wallet
./bin/wallet migrate up                      # aplica as migrations
./bin/wallet migrate down 1                  # reverte a última migration
./bin/wallet migrate version                 # versão atual

export DATABASE_URL=postgres://wallet_service:...@localhost:55432/wallet
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
| `AWS_REGION` / `AWS_ENDPOINT_URL` | `us-east-1` / — | Região e endpoint (LocalStack) |
| `SQS_CONSUMER_ACCESS_KEY_ID` / `SQS_CONSUMER_SECRET_ACCESS_KEY` | — | Credenciais do consumidor (padrão da AWS se vazio) |
| `SNS_PUBLISHER_ACCESS_KEY_ID` / `SNS_PUBLISHER_SECRET_ACCESS_KEY` | — | Credenciais do publisher |
| `SQS_INPUT_QUEUE_URL` / `SQS_DLQ_URL` | — (obrigatório com papel `consumer`) | Filas de entrada e dead-letter |
| `SQS_CONSUMER_WORKERS` / `SQS_MAX_MESSAGES` / `SQS_WAIT_TIME` | `4` / `10` / `20s` | Paralelismo e long polling |
| `SQS_PROCESSING_TIMEOUT` | `10s` | Prazo por mensagem (menor que a visibilidade da fila) |
| `SQS_RETRY_BASE_DELAY` / `SQS_RETRY_MAX_DELAY` | `2s` / `60s` | Backoff de visibilidade para falhas transitórias |
| `SNS_EVENTS_TOPIC_ARN` | — (obrigatório com papel `outbox`) | Destino dos eventos |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_LEASE` | `500ms` / `100` / `30s` | Publisher da outbox |
| `OUTBOX_PUBLISH_CONCURRENCY` | `8` | Carteiras publicadas em paralelo por lote (ordem preservada dentro de cada carteira) |
| `OUTBOX_RETRY_BASE_DELAY` / `OUTBOX_RETRY_MAX_DELAY` | `1s` / `5m` | Backoff de publicação |

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

## Filas e eventos

O LocalStack executa `deploy/localstack/init/ready.d/01-provision-messaging.sh` automaticamente e cria:
- as filas `wager-transactions.fifo` e `wager-transactions-dlq.fifo` (redrive após 5 recebimentos);
- o tópico `wallet-events.fifo`;
- a fila assinante `wallet-events-audit.fifo`;
- as identidades IAM de produtor, consumidor e publisher.

Detalhes em [ARCHITECTURE.md › Mensageria](ARCHITECTURE.md#mensageria).

Enviar uma operação pela fila (troque `WALLET_ID`/`PLAYER_ID`):

```sh
aws --endpoint-url http://localhost:4566 sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-123","idempotencyKey":"provider-a:transaction-123","playerId":"'$PLAYER_ID'","walletId":"'$WALLET_ID'","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}}'
```

Ler os eventos publicados e a DLQ:

```sh
aws --endpoint-url http://localhost:4566 sqs receive-message --max-number-of-messages 10 \
  --queue-url http://localhost:4566/000000000000/wallet-events-audit.fifo --message-attribute-names All
aws --endpoint-url http://localhost:4566 sqs receive-message --max-number-of-messages 10 \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo --message-attribute-names All
```

Os testes do consumidor (`internal/adapter/sqsconsumer`), do publisher (`internal/worker/outboxpub`) e da composição com todos os papéis (`internal/fxapp`) sobem **LocalStack e PostgreSQL reais**.

## Docker Compose

```sh
docker compose up --build -d --wait     # ou: make up
```

| Serviço | Endereço no host | Observação |
|---|---|---|
| `app-1`, `app-2`, `app-3` | API `:8091`, `:8092`, `:8093` · admin `:9091`, `:9092`, `:9093` | três instâncias independentes com todos os papéis (`api,consumer,outbox,pendingref`) |
| `postgres` | `localhost:55432` (`POSTGRES_HOST_PORT`) | porta alternativa para não colidir com um PostgreSQL local |
| `migrate` | — | executa `wallet migrate up` uma vez; as instâncias só sobem após sucesso |
| `keycloak` | `http://localhost:8081` | realm `wagering` importado no boot |
| `localstack` | `http://localhost:4566` | filas, tópico e identidades provisionados no boot |
| `jaeger` | `http://localhost:16686` | traces de HTTP, SQL, SQS e SNS |
| `prometheus` | `http://localhost:9090` | coleta `/metrics` das três instâncias |
| `grafana` | `http://localhost:3000` | acesso anônimo de leitura, datasource Prometheus provisionado |

Todas as instâncias compartilham o mesmo banco, fila e tópico: uma carteira aberta em `app-1` pode receber uma aposta em `app-2` e um replay em `app-3`. Migrations manuais via compose:

```sh
make migrate-version
make migrate-down N=1
make migrate-up
```

Para subir as instâncias com os pontos de falha compilados (ver abaixo): `BUILD_TAGS=faultinject docker compose up --build -d --wait`.

## Testes multi-instância e de falhas

A suíte `test/e2e` (build tag `e2e`) compila o binário com a tag `faultinject` e inicia **processos do sistema operacional independentes** contra o PostgreSQL, o Keycloak e o LocalStack do compose. Cada teste cria um banco próprio (migrado com `wallet migrate up`), filas, DLQ e tópico próprios, portanto os cenários são isolados e podem ser repetidos.

```sh
make up-infra
make test-e2e
```

| Cenário | Teste |
|---|---|
| Mesma aposta 50× distribuída entre 3 instâncias → um débito, 49 replays com o mesmo saldo | `TestIdenticalBetAcrossInstancesDebitsOnce` |
| Duas apostas de 80.00 sobre 100.00 em instâncias diferentes | `TestCompetingBetsAcrossInstancesNeverOverdraw` |
| 20 carteiras × apostas e ganhos concorrentes em 3 instâncias + reconciliação | `TestManyWalletsUnderConcurrentLoadStayConsistent` |
| Mesma operação via HTTP (10×) e SQS (5 mensagens) simultaneamente | `TestSameOperationThroughHTTPAndSQSIsAppliedOnce` |
| Consumer morre após o commit e antes do delete; outra instância recebe a reentrega | `TestConsumerCrashAfterCommitBeforeDeleteIsRedeliveredWithoutDoubleDebit` |
| Publisher morre entre publicar e confirmar; dois publishers retomam com o mesmo `eventId` | `TestPublisherCrashBetweenPublishAndConfirmRepublishesSameEventID` |
| Reversão pendente sobrevive a `kill -9` e é resolvida por outra instância | `TestPendingReferenceSurvivesKillAndIsResolvedByAnotherInstance` |
| Referência que nunca chega expira por TTL | `TestPendingReferenceExpiresWhenReferenceNeverArrives` |
| `kill -9` de uma instância durante carga, com retries do cliente | `TestKillDuringConcurrentLoadWithClientRetries` |
| `SIGTERM`, reinício e idempotência preservada (HTTP e SQS) | `TestGracefulShutdownCompletesAndRestartPreservesIdempotency` |
| Estado compartilhado entre `app-1..3` do compose (ignorado se não estiverem rodando) | `TestComposeInstancesShareState` |

Todos os cenários terminam verificando, direto no banco, que saldo = soma do ledger, que a versão da carteira bate com a última entrada e que nenhuma transação movimentou dinheiro duas vezes.

A injeção de falhas só existe em binários compilados com `-tags faultinject`; no build padrão os pontos são funções vazias. O ponto é escolhido por `FAULT_CRASH_POINT` e o processo termina com `os.Exit(86)`, sem shutdown gracioso, como em um `kill -9`:

| `FAULT_CRASH_POINT` | Momento |
|---|---|
| `sqs-after-commit-before-delete` | transação confirmada, mensagem ainda não removida da fila |
| `outbox-after-publish-before-confirm` | evento aceito pelo SNS, `published_at` ainda não gravado |

Variáveis opcionais da suíte: `E2E_POSTGRES_HOST`, `E2E_POSTGRES_OWNER`, `E2E_POSTGRES_OWNER_PASSWORD`, `E2E_POSTGRES_APP_USER`, `E2E_POSTGRES_APP_PASSWORD`, `E2E_KEYCLOAK_URL`, `E2E_LOCALSTACK_URL`, `E2E_COMPOSE_API_URLS`.

## Observabilidade local

| Ferramenta | Endereço | Conteúdo |
|---|---|---|
| Grafana | `http://localhost:3000` | dashboard **Wallet Service** (pasta Wallet) provisionado automaticamente: HTTP, processamento, idempotência, concorrência, SQS/DLQ, outbox, pendências, reconciliação e runtime |
| Prometheus | `http://localhost:9090` | métricas das três instâncias |
| Jaeger | `http://localhost:16686` | traces de HTTP, SQL, SQS e SNS (`OTEL_TRACES_SAMPLE_PERCENT` controla a amostragem, padrão 100) |

## Teste de carga

```sh
OTEL_TRACES_SAMPLE_PERCENT=10 docker compose up --build -d --wait
make load
```

O k6 roda dentro do compose (perfil `load`), com três cenários (liquidações completas, duplicatas simultâneas nas três instâncias e carteiras concorridas) e thresholds de latência, respostas fora do contrato e divergências de reconciliação. Resultados, ambiente de medição e os gargalos encontrados e corrigidos estão em [docs/LOAD_TEST.md](docs/LOAD_TEST.md).

Resumo do perfil base em um MacBook de 8 núcleos com toda a stack local: ~790 req/s, p95 de 57 ms em `BET`, 0 erros e 0 divergências em 87 mil lançamentos.

## Integração contínua

`.github/workflows/ci.yml` executa, a cada push na `main` e em pull requests:

| Job | Comando |
|---|---|
| Lint | `go mod verify` e `make lint` (gofmt, go vet com todas as build tags, golangci-lint) |
| Unit tests | `make test-cover` (`-race`, relatório de cobertura como artefato) |
| Integration tests | `make test-integration` (PostgreSQL, Keycloak e LocalStack via testcontainers) |
| Multi-instance and fault-injection tests | `make up` (stack completa com 3 instâncias) e `make test-e2e`; logs do compose em caso de falha |

