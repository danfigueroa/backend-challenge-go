# backend-challenge-go

Serviço em Go para **processamento distribuído de operações de apostas** sobre carteiras de jogadores: API HTTP autenticada e consumidor SQS que compartilham o mesmo caso de uso, com integridade financeira imposta pelo banco, idempotência persistente, coordenação por carteira entre várias instâncias, transactional inbox/outbox e recuperação de falhas.

| Documento | Conteúdo |
|---|---|
| [`docs/CHALLENGE.md`](docs/CHALLENGE.md) | enunciado |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | decisões técnicas, contratos, garantias, limitações e interpretações |
| [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md) | matriz de rastreabilidade: cada requisito → implementação → teste |
| [`docs/LOAD_TEST.md`](docs/LOAD_TEST.md) | teste de carga: metodologia, resultados e gargalos corrigidos |

## Destaques

- **Dinheiro** em `int64` de unidades mínimas, parsing estrito, overflow checado e `float` proibido por lint.
- **Invariantes no PostgreSQL**: `CHECK`, `UNIQUE` parciais, ledger append-only protegido por grants e triggers, constraint triggers adiados que conferem saldo × ledger × transação no commit, papéis com privilégios mínimos.
- **Concorrência** por carteira (`SELECT … FOR UPDATE` + versão), sem lock global; comprovada com **processos independentes**, `kill -9`, crash injetado entre commit e delete/confirmação e quedas de rede do banco e do broker.
- **Idempotência** persistente por chave + hash canônico do payload, equivalente entre HTTP e SQS, com replay do saldo original.
- **Mensageria** com inbox na mesma transação do domínio, DLQ explícita e por redrive, outbox com lease, backoff e publicação paralela por carteira preservando a ordem.
- **Observabilidade**: logs JSON correlacionados, métricas Prometheus, tracing OpenTelemetry (HTTP → SQL → SNS → SQS), dashboard Grafana provisionado e teste de carga k6.

## Pré-requisitos

| Ferramenta | Uso |
|---|---|
| Docker 27+ com Docker Compose v2 | stack local, testes de integração (testcontainers) e e2e |
| Go 1.27.1+ | build e testes fora de containers |
| GNU Make | atalhos (opcional; todos os comandos equivalentes estão documentados) |
| `curl` e `jq` | exemplos de chamadas |

Não é necessário instalar AWS CLI: os exemplos de fila usam o `awslocal` do container do LocalStack.

## Início rápido

```sh
docker compose up --build -d --wait       # PostgreSQL, migrations, Keycloak, LocalStack, 3 instâncias, Jaeger, Prometheus, Grafana
```

Obter tokens (`client_credentials` no Keycloak provisionado automaticamente):

```sh
KC=http://localhost:8081/realms/wagering/protocol/openid-connect/token
token() { curl -s -X POST "$KC" -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-local-secret" | jq -r .access_token; }
INTERNAL=$(token wallet-internal)
PROVIDER_A=$(token provider-a)
PROVIDER_B=$(token provider-b)
```

Abrir uma carteira na instância 1, apostar na instância 2 e repetir a mesma aposta na instância 3:

```sh
PLAYER_ID=$(uuidgen | tr 'A-Z' 'a-z')
WALLET_ID=$(curl -s -X POST http://localhost:8091/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d '{"playerId":"'$PLAYER_ID'","initialBalance":{"amount":"1000.00","currency":"BRL"}}' | jq -r .id)

BET='{"providerId":"provider-a","externalTransactionId":"transaction-123","playerId":"'$PLAYER_ID'","walletId":"'$WALLET_ID'","roundId":"round-987","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"}}'

curl -s -X POST http://localhost:8092/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d "$BET"
# {"transactionId":"...","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}

curl -s -X POST http://localhost:8093/wagering/transactions -H "Authorization: Bearer $PROVIDER_A" \
  -H 'Content-Type: application/json' -H 'Idempotency-Key: provider-a:transaction-123' -d "$BET"
# mesmo transactionId e saldo, "idempotentReplay":true
```

Consultas, isolamento entre provedores e reconciliação:

```sh
curl -s http://localhost:8091/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER_A"
curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8091/providers/provider-a/wagering/transactions/transaction-123 -H "Authorization: Bearer $PROVIDER_B"   # 403
curl -s http://localhost:8091/wallets/$WALLET_ID -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8092/wallets/$WALLET_ID/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
curl -s -X POST http://localhost:8093/wallets/$WALLET_ID/reconciliation -H "Authorization: Bearer $INTERNAL"
# {"storedBalance":{"amount":"975.00",...},"calculatedBalance":{"amount":"975.00",...},"difference":{"amount":"0.00",...},"consistent":true,"checkedEntries":2,...}
```

Enviar um `WIN` pela fila SQS referenciando a aposta:

```sh
docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" --message-deduplication-id msg-123 \
  --message-body '{"messageId":"msg-123","type":"WagerTransactionRequested","occurredAt":"2026-09-08T12:00:00.000Z","data":{"providerId":"provider-a","externalTransactionId":"transaction-124","idempotencyKey":"provider-a:transaction-124","playerId":"'$PLAYER_ID'","walletId":"'$WALLET_ID'","roundId":"round-987","gameId":"fortune-chimp","kind":"WIN","money":{"amount":"50.00","currency":"BRL"},"referenceExternalTransactionId":"transaction-123"}}'

curl -s http://localhost:8091/wallets/$WALLET_ID -H "Authorization: Bearer $INTERNAL"   # saldo 1025.00, version 3
```

Ler os eventos publicados pela outbox e a DLQ:

```sh
docker compose exec -T localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wallet-events-audit.fifo
docker compose exec -T localstack awslocal sqs receive-message --max-number-of-messages 10 --message-attribute-names All \
  --queue-url http://localhost:4566/000000000000/wager-transactions-dlq.fifo
```

Status e corpos de cada situação (200, 201, 202, 400, 401, 403, 404, 409, 413, 415, 422, 503) estão em [ARCHITECTURE.md › Contrato HTTP](ARCHITECTURE.md#contrato-http).

Encerrar e remover os dados: `docker compose down -v`.

## Comandos

| Objetivo | Make | Comando equivalente |
|---|---|---|
| Stack completa | `make up` | `docker compose up --build -d --wait` |
| Somente infraestrutura (e2e) | `make up-infra` | `docker compose up -d --wait postgres keycloak localstack` |
| Derrubar e limpar | `make down` | `docker compose down -v --remove-orphans` |
| Build do binário | `make build` | `CGO_ENABLED=0 go build -trimpath -o bin/wallet ./cmd/wallet` |
| Testes unitários | `make test` | `go test ./...` e `go test -race ./...` |
| Cobertura | `make test-cover` | `go test -race -coverprofile=coverage.out ./...` |
| Integração | `make test-integration` | `go test -race -tags=integration ./...` |
| Multi-instância e falhas | `make test-e2e` | `go test -race -tags=e2e ./test/e2e/...` |
| Fuzzing de `Money` | `make fuzz` | `go test -run=^$ -fuzz=FuzzParse -fuzztime=30s ./internal/domain/money` |
| Vet | `make vet` | `go vet ./...` e `go vet -tags=integration,e2e,faultinject ./...` |
| Lint completo | `make lint` | gofmt + vet + `golangci-lint run --build-tags=integration,e2e,faultinject ./...` |
| Migrations | `make migrate-up` / `make migrate-down N=1` / `make migrate-version` | `docker compose run --rm migrate migrate up` (ou `down 1`, `version`) |
| Carga | `make load` / `make load-stress` | `docker compose --profile load run --rm k6` |

> **macOS**: o detector de corrida não precisa de cgo no macOS. Se o linker falhar com `unknown architecture arm64e` (Command Line Tools desatualizadas em relação ao SDK), exporte `CGO_ENABLED=0`; o `Makefile` já faz isso no Darwin.

## Variáveis de ambiente

Valores de exemplo em [`.env.example`](.env.example) (apenas segredos locais). A configuração é validada inteira na inicialização, considerando os papéis habilitados, e todos os problemas são reportados de uma vez.

| Variável | Padrão | Descrição |
|---|---|---|
| `APP_ROLES` | `api,consumer,outbox,pendingref` | Papéis executados pela instância |
| `APP_INSTANCE_ID` | hostname | Identificador da instância (logs, dono dos leases) |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `APP_START_TIMEOUT` / `APP_SHUTDOWN_TIMEOUT` | `60s` / `30s` | Prazos de inicialização e encerramento |
| `HTTP_ADDR` / `ADMIN_ADDR` | `:8080` / `:9090` | API pública e servidor administrativo (`/metrics`, health) |
| `HTTP_*_TIMEOUT`, `HTTP_REQUEST_TIMEOUT`, `HTTP_MAX_BODY_BYTES` | ver `.env.example` | Limites do servidor HTTP |
| `DATABASE_URL` | — (obrigatório) | Conexão do serviço (`wallet_service`) |
| `DATABASE_MIGRATIONS_URL` | `DATABASE_URL` | Conexão do dono do schema para migrations |
| `DATABASE_MAX_CONNS` / `DATABASE_MIN_CONNS` | `20` / `2` | Tamanho do pool |
| `DATABASE_LOCK_TIMEOUT` / `DATABASE_STATEMENT_TIMEOUT` / `DATABASE_HEALTH_TIMEOUT` | `2s` / `5s` / `2s` | Timeouts |
| `RETRY_ATTEMPTS` / `RETRY_BASE_DELAY` / `RETRY_MAX_DELAY` | `5` / `20ms` / `500ms` | Retry de falhas transitórias |
| `PENDING_TTL` | `30m` | Prazo para uma referência chegar |
| `PENDING_BASE_DELAY` / `PENDING_MAX_DELAY` | `1s` / `60s` | Backoff exponencial das pendências |
| `PENDING_CLAIM_LEASE` / `PENDING_POLL_INTERVAL` / `PENDING_BATCH_SIZE` | `30s` / `1s` / `50` | Worker de pendências |
| `OTEL_TRACES_ENABLED` / `OTEL_EXPORTER_OTLP_ENDPOINT` / `OTEL_TRACES_SAMPLE_PERCENT` | `false` / — / `100` | Tracing OpenTelemetry |
| `AUTH_ISSUER` | — (obrigatório com papel `api`) | Valor esperado do `iss` dos tokens |
| `AUTH_JWKS_URL` | — (obrigatório com papel `api`) | Endpoint de chaves do IdP (pode usar a rede interna) |
| `AUTH_AUDIENCE` | `wallet-api` | Audience exigida |
| `AWS_REGION` / `AWS_ENDPOINT_URL` / `AWS_HEALTH_TIMEOUT` | `us-east-1` / — / `2s` | Região, endpoint (LocalStack) e timeout de verificação |
| `SQS_CONSUMER_ACCESS_KEY_ID` / `SQS_CONSUMER_SECRET_ACCESS_KEY` | — | Credenciais do consumidor (cadeia padrão da AWS se vazio) |
| `SNS_PUBLISHER_ACCESS_KEY_ID` / `SNS_PUBLISHER_SECRET_ACCESS_KEY` | — | Credenciais do publisher |
| `SQS_INPUT_QUEUE_URL` / `SQS_DLQ_URL` | — (obrigatório com papel `consumer`) | Filas de entrada e dead-letter |
| `SQS_CONSUMER_NAME` | `wager-transactions-consumer` | Identidade do consumidor na inbox |
| `SQS_CONSUMER_WORKERS` / `SQS_MAX_MESSAGES` / `SQS_WAIT_TIME` | `4` / `10` / `20s` | Paralelismo e long polling |
| `SQS_PROCESSING_TIMEOUT` | `10s` | Prazo por mensagem (menor que a visibilidade da fila) |
| `SQS_RETRY_BASE_DELAY` / `SQS_RETRY_MAX_DELAY` | `2s` / `60s` | Backoff de visibilidade para falhas transitórias |
| `SNS_EVENTS_TOPIC_ARN` | — (obrigatório com papel `outbox`) | Destino dos eventos |
| `OUTBOX_POLL_INTERVAL` / `OUTBOX_BATCH_SIZE` / `OUTBOX_LEASE` | `500ms` / `100` / `30s` | Publisher da outbox |
| `OUTBOX_PUBLISH_CONCURRENCY` | `8` | Carteiras publicadas em paralelo por lote (ordem preservada dentro de cada carteira) |
| `OUTBOX_RETRY_BASE_DELAY` / `OUTBOX_RETRY_MAX_DELAY` | `1s` / `5m` | Backoff de publicação |

Variáveis usadas apenas pelo Docker Compose: `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `APP_DB_USER`, `APP_DB_PASSWORD`, `POSTGRES_HOST_PORT` (padrão `55432`), `BUILD_TAGS`, `LOAD_*` (k6) e `FAULT_CRASH_POINT` (somente binários `faultinject`).

## Autenticação

O Keycloak importa o realm `wagering` (`deploy/keycloak/realm-wagering.json`) no boot, com clients `client_credentials` de teste. Os segredos existem apenas para uso local:

| Client | Segredo | Permissões |
|---|---|---|
| `provider-a` / `provider-b` | `provider-a-local-secret` / `provider-b-local-secret` | enviar e consultar transações do próprio provedor (`provider_id` fixo no token) |
| `wallet-internal` | `wallet-internal-local-secret` | abrir e consultar carteiras, ledger e reconciliação |
| `provider-a-short-lived` | `provider-a-short-lived-local-secret` | como `provider-a`, token de 2 s (teste de expiração) |
| `provider-unprivileged` | `provider-unprivileged-local-secret` | nenhuma |
| `foreign-audience` | `foreign-audience-local-secret` | token sem a audience `wallet-api` |

Modelo de permissões e validação dos tokens em [ARCHITECTURE.md › Autenticação e autorização](ARCHITECTURE.md#autenticação-e-autorização).

## Filas, eventos e migrations

O LocalStack executa `deploy/localstack/init/ready.d/01-provision-messaging.sh` no boot e cria:

- `wager-transactions.fifo` (visibilidade 30 s) e `wager-transactions-dlq.fifo`, com redrive após 5 recebimentos;
- o tópico `wallet-events.fifo` e a fila assinante `wallet-events-audit.fifo`;
- identidades IAM e políticas separadas para produtor, consumidor e publisher.

Contratos de mensagens e eventos em [ARCHITECTURE.md › Mensageria](ARCHITECTURE.md#mensageria).

As migrations (`migrations/`, embutidas no binário) são aplicadas pelo serviço `migrate` do compose antes das instâncias subirem. Aplicação e reversão manuais:

```sh
make migrate-version
make migrate-down N=1
make migrate-up
```

Fora do compose, com o binário:

```sh
make build
export DATABASE_MIGRATIONS_URL=postgres://wallet_owner:local-owner-password@localhost:55432/wallet?sslmode=disable
./bin/wallet migrate up
./bin/wallet migrate down 1
./bin/wallet migrate version

export DATABASE_URL=postgres://wallet_service:local-service-password@localhost:55432/wallet?sslmode=disable
./bin/wallet serve          # demais variáveis conforme a tabela acima; SIGTERM encerra graciosamente
```

## Docker Compose

| Serviço | Endereço no host | Observação |
|---|---|---|
| `app-1`, `app-2`, `app-3` | API `:8091`, `:8092`, `:8093` · admin `:9091`, `:9092`, `:9093` | três instâncias independentes com todos os papéis |
| `postgres` | `localhost:55432` (`POSTGRES_HOST_PORT`) | porta alternativa para não colidir com um PostgreSQL local |
| `migrate` | — | `wallet migrate up` uma vez; as instâncias só sobem após sucesso |
| `keycloak` | `http://localhost:8081` | realm `wagering` importado no boot |
| `localstack` | `http://localhost:4566` | filas, tópico e identidades provisionados no boot |
| `jaeger` | `http://localhost:16686` | traces de HTTP, SQL, SQS e SNS |
| `prometheus` | `http://localhost:9090` | coleta `/metrics` das três instâncias |
| `grafana` | `http://localhost:3000` | dashboard **Wallet Service** provisionado, acesso anônimo de leitura |
| `k6` | — | perfil `load`, só executa com `make load` |

Todas as instâncias compartilham banco, filas e tópico. Health checks: `GET /health/live` e `GET /health/ready` na API e no servidor administrativo; readiness cobre PostgreSQL, SQS (papel `consumer`) e SNS (papel `outbox`).

Para subir as instâncias com os pontos de falha compilados: `BUILD_TAGS=faultinject docker compose up --build -d --wait`.

## Testes

### Unitários

```sh
go test ./...
go test -race ./...
```

Domínio (`Money`, carteira, máquina de estados, cinco tipos externos, política de zero, `OPENING`, eventos, hash canônico com vetores golden), configuração, HTTP, auth, métricas e workers. Não exigem Docker.

### Integração

```sh
go test -race -tags=integration ./...
```

Exigem apenas o Docker em execução: testcontainers-go sobe **PostgreSQL 18.6, Keycloak 26.7.4 e LocalStack 4.14.0 reais** e os remove ao final. Cobrem migrations up/down, cada constraint e trigger violado diretamente em SQL, atomicidade, concorrência entre serviços, inbox, reentrega, DLQ, outbox concorrente, retry, autenticação real, isolamento entre provedores e a composição Fx completa com verificação de vazamento de goroutines.

### Multi-instância e simulação de falhas

```sh
docker compose up -d --wait postgres keycloak localstack     # make up-infra
go test -race -count=1 -tags=e2e ./test/e2e/...              # make test-e2e
```

A suíte compila o binário com a tag `faultinject` e inicia **processos do sistema operacional independentes** contra a infraestrutura do compose. Cada teste cria banco (migrado com `wallet migrate up`), filas, DLQ e tópico próprios, e termina verificando no banco que saldo = soma do ledger, que a versão da carteira bate com a última entrada e que nenhuma transação movimentou dinheiro duas vezes.

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
| Queda de rede do PostgreSQL: readiness 503, API 503 com `Retry-After`, mensagem SQS com retry e sem DLQ, recuperação sem duplicidade | `TestDatabaseOutageIsReportedAndRecoveredWithoutDuplicates` |
| Queda de rede do broker: API segue operando, consumo e publicação aguardam e retomam sem perda nem duplicidade de eventos | `TestBrokerOutageDelaysMessagingWithoutLosingEvents` |
| Estado compartilhado entre `app-1..3` do compose (ignorado se não estiverem rodando) | `TestComposeInstancesShareState` |

As quedas de rede usam um proxy TCP do próprio teste entre os processos e o PostgreSQL ou o LocalStack, que pode ser cortado (conexões abertas encerradas e novas recusadas) e restaurado.

A injeção de crash só existe em binários compilados com `-tags faultinject`; no build padrão os pontos são funções vazias. O ponto é escolhido por `FAULT_CRASH_POINT` e o processo termina com `os.Exit(86)`, sem shutdown gracioso, como em um `kill -9`:

| `FAULT_CRASH_POINT` | Momento |
|---|---|
| `sqs-after-commit-before-delete` | transação confirmada, mensagem ainda não removida da fila |
| `outbox-after-publish-before-confirm` | evento aceito pelo SNS, `published_at` ainda não gravado |

Variáveis opcionais da suíte: `E2E_POSTGRES_HOST`, `E2E_POSTGRES_OWNER`, `E2E_POSTGRES_OWNER_PASSWORD`, `E2E_POSTGRES_APP_USER`, `E2E_POSTGRES_APP_PASSWORD`, `E2E_KEYCLOAK_URL`, `E2E_LOCALSTACK_URL`, `E2E_COMPOSE_API_URLS`.

### Carga

```sh
docker compose down -v && OTEL_TRACES_SAMPLE_PERCENT=10 docker compose up --build -d --wait
make load
```

O k6 roda dentro do compose com três cenários (liquidações completas, duplicatas simultâneas nas três instâncias e carteiras concorridas) e thresholds de latência, respostas fora do contrato e divergências de reconciliação. Perfil base em um MacBook de 8 núcleos com toda a stack local: ~790 req/s, p95 de 57 ms em `BET`, 0 erros e 0 divergências em 87 mil lançamentos. Metodologia, perfil de estresse e gargalos encontrados em [docs/LOAD_TEST.md](docs/LOAD_TEST.md).

## Observabilidade

| Ferramenta | Endereço | Conteúdo |
|---|---|---|
| Logs | `docker compose logs -f app-1 app-2 app-3` | JSON com `correlationId`, `messageId`, `transactionId`, `walletId`, `providerId` |
| Grafana | `http://localhost:3000` | dashboard **Wallet Service**: HTTP, processamento, idempotência, concorrência, SQS/DLQ, outbox, pendências, reconciliação e runtime |
| Prometheus | `http://localhost:9090` | métricas das três instâncias |
| Jaeger | `http://localhost:16686` | traces de HTTP, SQL, SQS e SNS (`OTEL_TRACES_SAMPLE_PERCENT` controla a amostragem) |

## Integração contínua

`.github/workflows/ci.yml` executa a cada push na `main` e em pull requests:

| Job | Comando |
|---|---|
| Lint | `go mod verify` e `make lint` |
| Unit tests | `make test-cover` (`-race`, cobertura como artefato) |
| Integration tests | `make test-integration` |
| Multi-instance and fault-injection tests | `make up` e `make test-e2e`, com logs do compose em caso de falha |

## Estrutura

```
cmd/wallet/            binário único: serve, migrate, healthcheck
internal/domain/       modelo puro: money, wallet, wagering, event
internal/app/          casos de uso (walletapp, wageringapp), ports e autorização
internal/adapter/      postgres, httpapi, auth, awsclient, sqsconsumer
internal/worker/       runner, outbox publisher, resolvedor de pendências
internal/platform/     config, logging, metrics, tracing, health, httpserver, faultinject
internal/fxapp/        módulos Fx e seleção de papéis
internal/testsupport/  containers e harness dos testes de integração
migrations/            SQL versionado (up/down), embutido no binário
deploy/                postgres, keycloak, localstack, prometheus, grafana
test/e2e/              processos independentes, crash e quedas de rede
test/load/             cenários k6
docs/                  enunciado, rastreabilidade e teste de carga
```
