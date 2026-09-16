# Matriz de rastreabilidade de requisitos

Cada requisito de [`CHALLENGE.md`](CHALLENGE.md) mapeado para implementação e evidência de teste. Atualizada a cada entrega.

Legenda: ✅ concluído · 🚧 em andamento · ⏳ pendente

## Eliminatórios

| # | Critério eliminatório | Implementação | Evidência | Status |
|---|---|---|---|---|
| E1 | Autenticação efetiva nos endpoints de negócio | | | ⏳ |
| E2 | Sem acesso não autorizado a operações/transações | | | ⏳ |
| E3 | Sem cálculo monetário em ponto flutuante | `internal/domain/money`; `forbidigo` | `make lint`, `FuzzParse` | 🚧 |
| E4 | Sem saldo negativo por concorrência | | | ⏳ |
| E5 | Sem movimentação duplicada | | | ⏳ |
| E6 | Idempotência persistente (não em memória) | | | ⏳ |
| E7 | Funciona com múltiplas instâncias | | | ⏳ |
| E8 | Sem publicação anterior ao commit | | | ⏳ |
| E9 | Ledger auditável | `ledger_entries` append-only, encadeado, versionado | `TestLedgerConstraints`, `TestLedgerRepository` | 🚧 |
| E10 | PostgreSQL, SQS e IdP reais nos testes | PostgreSQL via testcontainers ✅; SQS e IdP nas próximas fases | `internal/adapter/postgres/*_integration_test.go` | 🚧 |

## Garantias obrigatórias (§5)

| # | Garantia | Implementação | Evidência | Status |
|---|---|---|---|---|
| G1 | Dinheiro sem `float32`/`float64` | `int64` em `money`; `forbidigo` no `.golangci.yml` | `make lint` | 🚧 |
| G2 | Idempotência persistente e resistente a reinício | | | ⏳ |
| G3 | Invariantes financeiras garantidas no banco | Migrations `000002`–`000004` (constraints, triggers, constraint triggers adiados) | `TestWalletConstraints`, `TestLedgerConstraints`, `TestTransactionConstraints` | ✅ |
| G4 | Publicação só após commit | | | ⏳ |
| G5 | Ledger append-only | Grants sem `UPDATE`/`DELETE` + triggers `ledger_entries_append_only` | `TestLedgerConstraints/append_only_*` | ✅ |
| G6 | Carteiras independentes em paralelo; sem lock global | Lock de linha por carteira | `TestSameWalletSerializesAndDistinctWalletsProceedInParallel` | 🚧 |
| G7 | Sem lost updates | `FOR UPDATE` + `WHERE version = $old` + trigger de versão + verificação adiada | `TestWalletRepository/stale_version…`, `TestConcurrentBetsOnSameWalletAtRepositoryLevel` | 🚧 |
| G8 | Unicidade, não negatividade e imutabilidade no schema | Ver ARCHITECTURE › Invariantes impostas pelo banco | `schema_integration_test.go` | ✅ |

## Stack e composição (§4)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| S1 | Versão Go declarada em `go.mod` e Dockerfile | `go.mod` (`go 1.27.1`) | | 🚧 |
| S2 | `go.mod`/`go.sum` versionados | `go.mod` | | 🚧 |
| S3 | Uber Fx com `fx.Module`/`fx.Provide`/`fx.Invoke` | | | ⏳ |
| S4 | `fx.Lifecycle`: validação no start, workers canceláveis, shutdown ordenado | | | ⏳ |
| S5 | Domínio independente de Fx/HTTP/SQS/persistência | `internal/domain` (depende só da stdlib e `google/uuid`) | `go list -deps ./internal/domain/...` | 🚧 |
| S6 | Migrations versionadas com up/down documentados | `migrations/`, `postgres.Migrator` | `TestMigrationsApplyRevertAndReapply` | 🚧 |
| S7 | Docker Compose | | | ⏳ |

## Domínio (§6, §7)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| D1 | Money: parsing estrito, zero, soma, subtração, negação, comparação, serialização | `internal/domain/money` | `money_test.go`, `json_test.go`, `FuzzParse` | ✅ |
| D2 | Money: overflow em parsing/soma/subtração/negação | `money.go` | `TestOverflow`, `TestParse`, `TestParseSigned` | ✅ |
| D3 | Money: incompatibilidade de moedas | `money.go` (`compatible`) | `TestCurrencyMismatch` | ✅ |
| D4 | Wallet: criação, reidratação, débito/crédito, versão | `internal/domain/wallet/wallet.go` | `wallet_test.go` | ✅ |
| D5 | WagerTransaction: máquina de estados e terminalidade | `internal/domain/wagering/transaction.go`, `status.go` | `TestStatusTransitions`, `TestTerminalTransactionsRejectTransitions`, `TestRehydrateValidation` | ✅ |
| D6 | OPENING interno, rejeitado em HTTP/SQS | `NewOpening`, `parseExternalKind` | `TestNewOpening`, `TestNewRequestValidation/opening_kind`, `TestOpeningEvents` | 🚧 |
| D7 | LedgerEntry imutável com `balanceAfter = balanceBefore ± money` | Domínio `ledger_entry.go` + `CHECK ledger_entries_arithmetic` | `ledger_entry_test.go`, `TestLedgerConstraints` | ✅ |
| D8 | Regras BET/WIN/LOSS/REFUND/ROLLBACK e política de zero | `wagering/processor.go`, `request.go` | `TestProcess*`, `TestZeroAmountPolicy`, `TestReferenceRules` | ✅ |
| D9 | Reversão única e combinação REFUND/ROLLBACK | Domínio `AlreadyReversed` + índice `wager_transactions_single_reversal` | `TestRefundAndRollbackCombinations`, `TestReversalUniquenessIsEnforcedByTheDatabase` | ✅ |
| D10 | Reversão sem saldo com código distinto | `REVERSAL_INSUFFICIENT_FUNDS` | `TestReversalInsufficientFundsUsesDistinctCode` | ✅ |
| D11 | `PENDING_REFERENCE` com backoff, limite e rejeição por expiração | Domínio: `PendingPolicy`, `awaitOrExpire`; worker na Fase 6 | `TestPendingReferenceLifecycle`, `TestPendingReferenceExpires*`, `TestPendingPolicyDelay` | 🚧 |
| D12 | `failureCode` estáveis e documentados | `wagering/failure_code.go`; ARCHITECTURE › Códigos de falha | `TestParsers`, `TestNewRequestValidation` | ✅ |
| D13 | Inbox e outbox | Tabelas + `InboxRepository`/`OutboxRepository` (workers na Fase 6) | `TestInboxRepository`, `TestOutboxRepository` | 🚧 |

## API HTTP (§9) e autenticação (§2)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| H1 | `POST /wallets` com OPENING, ledger e outbox atômicos; conflito em duplicata | | | ⏳ |
| H2 | `GET /wallets/:id` | | | ⏳ |
| H3 | `GET /wallets/:id/ledger` com cursor opaco | | | ⏳ |
| H4 | `GET /wagering/transactions/:id` | | | ⏳ |
| H5 | `GET /providers/:providerId/wagering/transactions/:externalId` | | | ⏳ |
| H6 | `POST /wagering/transactions` com `Idempotency-Key` obrigatório | | | ⏳ |
| H7 | Hash canônico e equivalência HTTP/SQS | `wagering.NewRequest` (ponto único), `CanonicalPayload` | `TestCanonicalPayloadGolden`, `TestCanonicalPayloadMatchesSortedJSONOracle`, `TestPayloadHash*` | 🚧 |
| H8 | Replay devolve saldo original com `idempotentReplay: true` | | | ⏳ |
| H9 | Conflitos de chave e de `(providerId, externalTransactionId)` | | | ⏳ |
| H10 | Códigos HTTP distinguíveis documentados | | | ⏳ |
| H11 | `POST /wallets/:id/reconciliation` | | | ⏳ |
| H12 | `/health/live` e `/health/ready` | | | ⏳ |
| A1 | IdP OIDC externo (Keycloak) provisionado automaticamente | | | ⏳ |
| A2 | `providerId` determinado pela identidade | | | ⏳ |
| A3 | Isolamento entre provedores (consultas e replays) | | | ⏳ |
| A4 | Operações de carteira restritas ao serviço interno | | | ⏳ |
| A5 | Credenciais e políticas do broker | | | ⏳ |

## Mensageria (§10, §11)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| M1 | Filas `wager-transactions.fifo` e DLQ com redrive | | | ⏳ |
| M2 | Inbox na mesma transação do domínio | | | ⏳ |
| M3 | Delete somente após commit | | | ⏳ |
| M4 | Retry com backoff; DLQ para permanentes/esgotados | | | ⏳ |
| M5 | Shutdown: para de buscar, conclui ou libera visibilidade | | | ⏳ |
| M6 | `MessageGroupId`/`MessageDeduplicationId` documentados | | | ⏳ |
| M7 | Outbox publisher com múltiplas instâncias, lease e backoff | | | ⏳ |
| M8 | Republicação preserva `eventId` | | | ⏳ |
| M9 | Quatro eventos com envelope tipado | `internal/domain/event` | `events_test.go` (golden JSON) | ✅ |
| M10 | Destino dos eventos provisionado e documentado | Validado: LocalStack 4.14.0, SNS FIFO → SQS FIFO | | 🚧 |

## Observabilidade (§12)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| O1 | Logs JSON com identificadores de rastreio, sem dados sensíveis | | | ⏳ |
| O2 | Métricas: status, duplicatas, retries, DLQ, conflitos, atraso outbox, latência, divergências | | | ⏳ |
| O3 | Tracing OpenTelemetry (opcional) | | | ⏳ |
| O4 | Dashboard Grafana (opcional) | | | ⏳ |

## Verificação (§13)

| # | Cenário | Teste | Status |
|---|---|---|---|
| T1 | Unitários de Money, Wallet, estados, cinco tipos, conflito de payload, zero, OPENING | `internal/domain/{money,wallet,wagering,event}` (cobertura 94–99%) | ✅ |
| T2 | Integração: migrations, constraints, imutabilidade, atomicidade | `migrations_integration_test.go`, `schema_integration_test.go`, `txmanager_integration_test.go` | ✅ |
| T3 | Integração: inbox, reentrega, outbox concorrente, retry, DLQ, reinício | Repositório: `TestInboxConcurrentDeliveriesInsertOnce`, `TestClaimDuePendingIsExclusiveAcrossWorkers`; SQS na Fase 6 | 🚧 |
| T4 | Composição Fx: start/stop e liberação de recursos | | ⏳ |
| T5 | Auth: credenciais ausentes/inválidas/expiradas; isolamento; sem efeitos | | ⏳ |
| T6 | Mesma aposta 50× em paralelo → um débito | | ⏳ |
| T7 | Duas apostas de 80.00 sobre 100.00 | Repositório: `TestConcurrentBetsOnSameWalletAtRepositoryLevel` (HTTP/SQS e multi-instância nas próximas fases) | 🚧 |
| T8 | Carteiras distintas em paralelo | Repositório: `TestSameWalletSerializesAndDistinctWalletsProceedInParallel` | 🚧 |
| T9 | Três instâncias independentes | | ⏳ |
| T10 | Consumer interrompido após commit e antes do delete | | ⏳ |
| T11 | Dois publishers disputando a outbox | Repositório: `TestOutboxConcurrentPublishersNeverShareRecords`, `TestOutboxRepository` (lease abandonado); worker na Fase 6 | 🚧 |
| T12 | Reversão antes da referência: resolução e expiração | | ⏳ |
| T13 | Reinício preserva idempotência, pendências e consistência | | ⏳ |
| T14 | Cenários cruzando HTTP e SQS | | ⏳ |
| T15 | Reconciliação final saldo × ledger | | ⏳ |
| T16 | Teste de carga k6 (opcional) | | ⏳ |

## Entrega (§15)

| # | Item | Status |
|---|---|---|
| X1 | `README.md` completo | ⏳ |
| X2 | `ARCHITECTURE.md` com todas as decisões e limitações | ⏳ |
| X3 | `.env.example` | ⏳ |
| X4 | `docker compose up --build`, `go test ./...`, `go test -race ./...`, `go vet ./...` | ⏳ |
| X5 | Instruções de integração, multi-instância e falhas | ⏳ |
| X6 | Código formatado com `gofmt` | ⏳ |
