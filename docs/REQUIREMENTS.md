# Matriz de rastreabilidade de requisitos

Cada requisito de [`CHALLENGE.md`](CHALLENGE.md) mapeado para implementação e evidência verificável (teste, comando ou artefato). Os nomes de teste citados existem no repositório; `Test*` indica um grupo de testes com o mesmo prefixo.

Legenda: ✅ atendido e verificado · ◐ atendido com limitação documentada

| Suíte | Comando | Infraestrutura |
|---|---|---|
| Unitários | `go test -race ./...` | nenhuma |
| Integração | `go test -race -tags=integration ./...` | Docker (testcontainers) |
| Multi-instância e falhas | `go test -race -tags=e2e ./test/e2e/...` | `docker compose up -d --wait postgres keycloak localstack` |
| Carga | `make load` | stack completa |

## Eliminatórios

| # | Critério eliminatório | Implementação | Evidência | Status |
|---|---|---|---|---|
| E1 | Autenticação efetiva nos endpoints de negócio | `internal/adapter/auth`, `httpapi.authenticate` | `TestAuthenticationAgainstRealKeycloak`, `TestProcessTransactionErrors` | ✅ |
| E2 | Sem acesso não autorizado a operações/transações | Roles na borda + `app.Actor` nos casos de uso | `TestProviderIsolationOverHTTP`, `TestProviderIsolation`, `TestActorAuthorization` | ✅ |
| E3 | Sem cálculo monetário em ponto flutuante | `internal/domain/money` (`int64`); `forbidigo` bloqueia `float32`/`float64` | `make lint`, `FuzzParse`; `float` só aparece em métricas e na taxa de amostragem de tracing | ✅ |
| E4 | Sem saldo negativo por concorrência | Lock por carteira + versão + `CHECK (balance_minor >= 0)` | `TestTwoConcurrentBetsOfEightyOnHundred`, `TestCompetingBetsAcrossInstancesNeverOverdraw` (3 processos), `TestKillDuringConcurrentLoadWithClientRetries` | ✅ |
| E5 | Sem movimentação duplicada | Idempotência + inbox + `UNIQUE (wallet_id, transaction_id)` | `TestSameBetFiftyTimesInParallelDebitsOnce`, `TestIdenticalBetAcrossInstancesDebitsOnce`, `TestSameOperationThroughHTTPAndSQSIsAppliedOnce`, `TestConsumerCrashAfterCommitBeforeDeleteIsRedeliveredWithoutDoubleDebit` | ✅ |
| E6 | Idempotência persistente (não em memória) | `wager_transactions` (chave, hash, resultado) | `TestReplayAfterRestartUsesPersistedState` | ✅ |
| E7 | Funciona com múltiplas instâncias | Estado apenas no PostgreSQL; claims com `SKIP LOCKED` + lease; compose com 3 instâncias | `test/e2e` (processos independentes), `TestComposeInstancesShareState` | ✅ |
| E8 | Sem publicação anterior ao commit | Outbox na mesma transação; publisher lê apenas linhas confirmadas | `TestEventsSurviveCrashBetweenCommitAndPublication`, `TestAllRolesProcessSQSMessagesAndPublishEvents` | ✅ |
| E9 | Ledger auditável | `ledger_entries` append-only (grants + triggers), encadeado e versionado; reconciliação | `TestLedgerConstraints`, `TestLedgerRepository`, `TestReconciliationDetectsDivergenceWithoutChangingBalance` | ✅ |
| E10 | PostgreSQL, SQS e IdP reais nos testes | testcontainers: PostgreSQL 18, Keycloak 26.7.4, LocalStack 4.14.0 | `*_integration_test.go` | ✅ |

## Garantias obrigatórias (§5)

| # | Garantia | Implementação | Evidência | Status |
|---|---|---|---|---|
| G1 | Dinheiro sem `float32`/`float64` | `int64` em `money`, `BIGINT` no banco, strings no JSON; `forbidigo` no `.golangci.yml` | `make lint`, `FuzzParse` | ✅ |
| G2 | Idempotência persistente e resistente a reinício | Chave + hash + resultado persistidos | `TestReplayAfterRestartUsesPersistedState`, `TestPendingSurvivesRestartAndIsResumedByAnotherInstance`, `TestGracefulShutdownCompletesAndRestartPreservesIdempotency` | ✅ |
| G3 | Invariantes financeiras garantidas no banco | Migrations `000002`–`000004` e `000007` (constraints, triggers, constraint triggers adiados com buscas indexadas) | `TestWalletConstraints`, `TestLedgerConstraints`, `TestTransactionConstraints`, `TestIntegrityTriggersReadLedgerByIndexedLookups` | ✅ |
| G4 | Publicação só após commit | Transactional outbox + publisher separado que só lê linhas confirmadas | `TestEventsSurviveCrashBetweenCommitAndPublication`, `TestBrokerOutageDelaysMessagingWithoutLosingEvents` | ✅ |
| G5 | Ledger append-only | Grants sem `UPDATE`/`DELETE` + triggers `ledger_entries_append_only` | `TestLedgerConstraints/append_only_*` | ✅ |
| G6 | Carteiras independentes em paralelo; sem lock global | Lock de linha por carteira | `TestSameWalletSerializesAndDistinctWalletsProceedInParallel`, `TestDistinctWalletsAreProcessedInParallel`, `TestManyWalletsUnderConcurrentLoadStayConsistent` | ✅ |
| G7 | Sem lost updates | `FOR UPDATE` + `WHERE version = $old` + trigger de versão + verificação adiada | `TestWalletRepository/stale_version_is_a_concurrent_update`, `TestConcurrentBetsOnSameWalletAtRepositoryLevel` | ✅ |
| G8 | Unicidade, não negatividade e imutabilidade no schema | Ver ARCHITECTURE › Invariantes impostas pelo banco | `schema_integration_test.go` | ✅ |

## Ambiente de execução e falhas (§3)

| # | Situação | Tratamento | Evidência | Status |
|---|---|---|---|---|
| F1 | Mesma operação recebida repetidamente, inclusive por HTTP e SQS | Chave + hash, inbox, double-check sob lock | `TestConcurrentHTTPAndSQSForTheSameOperation`, `TestSameOperationThroughHTTPAndSQSIsAppliedOnce`, `TestSQSDeliveriesAreDeduplicatedByInboxAndIdempotency` | ✅ |
| F2 | Reversão antes da referência | `PENDING_REFERENCE` + worker com backoff e TTL; despertar quando a referência termina | `TestReversalArrivingBeforeReferenceIsResolvedLater`, `TestReversalWaitingOnRejectedReferenceIsWokenAndRejected`, `TestPendingReferenceExpiresWhenReferenceNeverArrives` | ✅ |
| F3 | Operações simultâneas da mesma carteira | Lock de linha + versão + constraints | `TestTwoConcurrentBetsOfEightyOnHundred`, `TestIdenticalBetAcrossInstancesDebitsOnce` | ✅ |
| F4 | Encerramento abrupto antes ou depois do commit | Commit único; delete/confirmação só após commit; lease | `TestKillDuringConcurrentLoadWithClientRetries`, `TestConsumerCrashAfterCommitBeforeDeleteIsRedeliveredWithoutDoubleDebit`, `TestPublisherCrashBetweenPublishAndConfirmRepublishesSameEventID` | ✅ |
| F5 | Publicação repetida de um evento | `MessageDeduplicationId = eventId` estável | `TestRecoveryBetweenPublicationAndConfirmationKeepsEventID`, `TestPublisherCrashBetweenPublishAndConfirmRepublishesSameEventID` | ✅ |
| F6 | Indisponibilidade temporária do PostgreSQL ou do SQS | Retry classificado, 503 + `Retry-After`, backoff de visibilidade, readiness, outbox retida | `TestDatabaseOutageIsReportedAndRecoveredWithoutDuplicates`, `TestBrokerOutageDelaysMessagingWithoutLosingEvents`, `TestTxManagerLockTimeoutIsTransient`, `TestTransientFailuresAreRetriedThenRedriven` | ✅ |

## Stack e composição (§4)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| S1 | Versão Go declarada em `go.mod` e Dockerfile | `go.mod` (`go 1.27.1`), `Dockerfile` (`golang:1.27.1-alpine3.24`) | `docker build` | ✅ |
| S2 | `go.mod`/`go.sum` versionados | `go.mod`, `go.sum` | `go mod verify` | ✅ |
| S3 | Uber Fx com `fx.Module`/`fx.Provide`/`fx.Invoke` | `internal/fxapp` | `TestApplicationGraphIsValidForEveryRoleCombination` | ✅ |
| S4 | `fx.Lifecycle`: validação no start, workers canceláveis, shutdown ordenado | `internal/fxapp`, `internal/worker`, `internal/platform/httpserver` | `TestApplicationStartsServesAndStopsCleanly`, `TestRunner*`, `TestServerLifecycleCompletesInFlightRequests`, `TestGracefulShutdownCompletesAndRestartPreservesIdempotency` | ✅ |
| S5 | Domínio independente de Fx/HTTP/SQS/persistência | `internal/domain` (depende só da stdlib e `google/uuid`) | `go list -deps ./internal/domain/...` | ✅ |
| S6 | Migrations versionadas com up/down documentados | `migrations/`, `wallet migrate up|down|version`, README | `TestMigrationsApplyRevertAndReapply` | ✅ |
| S7 | Docker Compose | `docker-compose.yml`: PostgreSQL, migrate, Keycloak, LocalStack, `app-1..3`, Jaeger, Prometheus, Grafana | `docker compose up --build --wait`, `TestComposeInstancesShareState` | ✅ |

## Domínio (§6, §7)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| D1 | Money: parsing estrito, zero, soma, subtração, negação, comparação, serialização | `internal/domain/money` | `money_test.go`, `json_test.go`, `FuzzParse` | ✅ |
| D2 | Money: overflow em parsing/soma/subtração/negação | `money.go` | `TestOverflow`, `TestParse`, `TestParseSigned` | ✅ |
| D3 | Money: incompatibilidade de moedas | `money.go` (`compatible`) | `TestCurrencyMismatch` | ✅ |
| D4 | Wallet: criação, reidratação, débito/crédito, versão | `internal/domain/wallet/wallet.go` | `wallet_test.go` | ✅ |
| D5 | WagerTransaction: máquina de estados e terminalidade | `internal/domain/wagering/transaction.go`, `status.go` | `TestStatusTransitions`, `TestTerminalTransactionsRejectTransitions`, `TestRehydrateValidation` | ✅ |
| D6 | OPENING interno, rejeitado em HTTP/SQS | `NewOpening`, `parseExternalKind`, `walletapp.OpenWallet` | `TestNewOpening`, `TestOpenWalletWithPositiveBalance`, `TestCorrectableErrorsAreNotPersisted` | ✅ |
| D7 | LedgerEntry imutável com `balanceAfter = balanceBefore ± money` | Domínio `ledger_entry.go` + `CHECK ledger_entries_arithmetic` | `ledger_entry_test.go`, `TestLedgerConstraints` | ✅ |
| D8 | Regras BET/WIN/LOSS/REFUND/ROLLBACK e política de zero | `wagering/processor.go`, `request.go` | `TestProcess*`, `TestZeroAmountPolicy`, `TestReferenceRules` | ✅ |
| D9 | Reversão única e combinação REFUND/ROLLBACK | Domínio `AlreadyReversed` + índice `wager_transactions_single_reversal` | `TestRefundAndRollbackCombinations`, `TestReversalUniquenessIsEnforcedByTheDatabase` | ✅ |
| D10 | Reversão sem saldo com código distinto | `REVERSAL_INSUFFICIENT_FUNDS` | `TestReversalInsufficientFundsUsesDistinctCode` | ✅ |
| D11 | `PENDING_REFERENCE` com backoff, limite e rejeição por expiração | `PendingPolicy`, `ResolveDuePending`, `internal/worker/pendingref` | `TestReversalArrivingBeforeReferenceIsResolvedLater`, `TestPendingReferenceExpiresWithRejection`, `TestPendingReferenceExpiresWhenReferenceNeverArrives` | ✅ |
| D12 | `failureCode` estáveis e documentados | `wagering/failure_code.go`; ARCHITECTURE › Códigos de falha | `TestParsers`, `TestNewRequestValidation` | ✅ |
| D13 | Inbox e outbox | Tabelas, repositórios, consumidor e publisher | `TestInboxRepository`, `TestOutboxRepository`, `sqsconsumer`/`outboxpub` integration tests | ✅ |

## API HTTP (§9) e autenticação (§2)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| H1 | `POST /wallets` com OPENING, ledger e outbox atômicos; conflito em duplicata | `walletapp.OpenWallet`, `httpapi.openWallet` | `TestOpenWallet*`, `TestHTTPContractEndToEnd` | ✅ |
| H2 | `GET /wallets/:id` | `httpapi.getWallet` | `TestWalletEndpoints`, `TestHTTPContractEndToEnd` | ✅ |
| H3 | `GET /wallets/:id/ledger` com cursor opaco | `ListLedger` + `httpapi.listLedger` | `TestGetWalletAndLedgerPagination`, `TestHTTPContractEndToEnd` | ✅ |
| H4 | `GET /wagering/transactions/:id` | `httpapi.getTransaction` | `TestTransactionQueries`, `TestProviderIsolationOverHTTP` | ✅ |
| H5 | `GET /providers/:providerId/wagering/transactions/:externalId` | `httpapi.getByExternalID` | `TestTransactionQueries`, `TestProviderIsolationOverHTTP` | ✅ |
| H6 | `POST /wagering/transactions` com `Idempotency-Key` obrigatório | `httpapi.processTransaction` | `TestProcessTransactionErrors/missing_idempotency_key`, `TestHTTPContractEndToEnd` | ✅ |
| H7 | Hash canônico e equivalência HTTP/SQS | `wagering.NewRequest` usado pelas duas portas | `TestCanonicalPayloadGolden`, `TestSQSDeliveriesAreDeduplicatedByInboxAndIdempotency`, `TestSameOperationThroughHTTPAndSQSIsAppliedOnce` | ✅ |
| H8 | Replay devolve saldo original com `idempotentReplay: true` | Resultado persistido | `TestBetIsProcessedAndReplayReturnsOriginalBalance`, `TestHTTPContractEndToEnd` | ✅ |
| H9 | Conflitos de chave e de `(providerId, externalTransactionId)` | `IdempotencyConflictError` → 409 | `TestIdempotencyConflicts`, `TestHTTPContractEndToEnd` | ✅ |
| H10 | Códigos HTTP distinguíveis documentados | ARCHITECTURE › Contrato HTTP | `TestProcessTransactionResponses`, `TestProcessTransactionErrors` | ✅ |
| H11 | `POST /wallets/:id/reconciliation` | `Reconcile` + `httpapi.reconcile` | `TestReconciliation*`, `TestHTTPContractEndToEnd` | ✅ |
| H12 | `/health/live` e `/health/ready` | API pública e admin; readiness de PostgreSQL, SQS e SNS por papel | `TestPublicHealthAndRoutingAndRecovery`, `TestAllRolesProcessSQSMessagesAndPublishEvents` | ✅ |
| A1 | IdP OIDC externo (Keycloak) provisionado automaticamente | `deploy/keycloak/realm-wagering.json` (`--import-realm`) | `kctest` + `TestAuthenticationAgainstRealKeycloak` | ✅ |
| A2 | `providerId` determinado pela identidade | Claim fixa `provider_id` → `app.ProviderActor` | `TestProviderIsolationOverHTTP`, `TestPrincipalActorMapping` | ✅ |
| A3 | Isolamento entre provedores (consultas e replays) | Roles + `app.Actor` | `TestProviderIsolationOverHTTP`, `TestProviderIsolation` | ✅ |
| A4 | Operações de carteira restritas ao serviço interno | Roles `wallets:*` + `RequireInternalService` | `TestProviderIsolationOverHTTP`, `TestOpenWalletConflictAndValidation` | ✅ |
| A5 | Credenciais e políticas do broker | Credenciais por componente, IAM + políticas de recurso provisionadas; LocalStack community registra mas não aplica IAM (ver ARCHITECTURE › Credenciais e políticas do broker) | `TestProvisionedMessagingTopology` | ◐ |

## Mensageria (§10, §11)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| M1 | Filas `wager-transactions.fifo` e DLQ com redrive | `deploy/localstack/init/ready.d/01-provision-messaging.sh` | `TestProvisionedMessagingTopology` | ✅ |
| M2 | Inbox na mesma transação do domínio | `wageringapp.Process` com `Delivery` | `TestSQSDeliveriesAreDeduplicatedByInboxAndIdempotency`, `TestRedeliveredMessageIsDeduplicated` | ✅ |
| M3 | Delete somente após commit | `sqsconsumer.complete` | `TestConsumesMessageAndDeletesAfterCommit`, `TestCrashAfterCommitBeforeDeleteIsRedeliveredSafely` | ✅ |
| M4 | Retry com backoff; DLQ para permanentes/esgotados | Visibilidade com backoff + redrive; DLQ explícita com motivo | `TestTransientFailuresAreRetriedThenRedriven`, `TestPermanentFailuresAreDeadLettered` | ✅ |
| M5 | Shutdown: para de buscar, conclui ou libera visibilidade | `Consumer.release` | `TestShutdownCompletesInFlightAndReleasesTheRest` | ✅ |
| M6 | `MessageGroupId`/`MessageDeduplicationId` documentados | ARCHITECTURE › Consumidor SQS / Contrato de roteamento | `TestConcurrentHTTPAndSQSForTheSameOperation` | ✅ |
| M7 | Outbox publisher com múltiplas instâncias, lease e backoff | `internal/worker/outboxpub` (publicação paralela por carteira) | `TestConcurrentPublishersPublishEachEventOnce`, `TestFailedPublicationIsRetriedWithBackoff`, `TestShutdownReleasesClaimedEvents`, `TestPartitionsArePublishedConcurrentlyInOrder`, `TestFailedPublicationPostponesOnlyItsPartition` | ✅ |
| M8 | Republicação preserva `eventId` | `MessageDeduplicationId = eventId` | `TestRecoveryBetweenPublicationAndConfirmationKeepsEventID` | ✅ |
| M9 | Quatro eventos com envelope tipado | `internal/domain/event` | `events_test.go` (golden JSON) | ✅ |
| M10 | Destino dos eventos provisionado e documentado | SNS FIFO `wallet-events.fifo` → `wallet-events-audit.fifo` | `TestProvisionedMessagingTopology`, `TestAllRolesProcessSQSMessagesAndPublishEvents` | ✅ |

## Observabilidade (§12)

| # | Requisito | Implementação | Evidência | Status |
|---|---|---|---|---|
| O1 | Logs JSON com identificadores de rastreio, sem dados sensíveis | `internal/platform/logging` | `logging_test.go` | ✅ |
| O2 | Métricas: status, duplicatas, retries, DLQ, conflitos, atraso outbox, latência, divergências | `internal/platform/metrics` + instrumentação de consumidor e publisher | `metrics_test.go`, `TestAllRolesProcessSQSMessagesAndPublishEvents` | ✅ |
| O3 | Tracing OpenTelemetry (opcional) | `otelpgx`, `otelhttp`, `otelaws`, propagação `traceparent` via atributos SQS | `tracing_test.go` | ✅ |
| O4 | Dashboard Grafana (opcional) | `deploy/grafana/dashboards/wallet-service.json` provisionado | `docker compose up`, Grafana › Wallet › Wallet Service | ✅ |

## Verificação (§13)

| # | Cenário | Teste | Status |
|---|---|---|---|
| T1 | Unitários de Money, Wallet, estados, cinco tipos, conflito de payload, zero, OPENING | `internal/domain/{money,wallet,wagering,event}` (cobertura 94–99%) | ✅ |
| T2 | Integração: migrations, constraints, imutabilidade, atomicidade | `migrations_integration_test.go`, `schema_integration_test.go`, `txmanager_integration_test.go` | ✅ |
| T3 | Integração: inbox, reentrega, outbox concorrente, retry, DLQ, reinício | `sqsconsumer` e `outboxpub` integration tests (LocalStack real) | ✅ |
| T4 | Composição Fx: start/stop e liberação de recursos | `TestApplicationStartsServesAndStopsCleanly` (goleak), `TestApplicationFailsToStartWithoutDatabase` | ✅ |
| T5 | Auth: credenciais ausentes/inválidas/expiradas; isolamento; sem efeitos | `TestAuthenticationAgainstRealKeycloak`, `TestProviderIsolationOverHTTP` (Keycloak real), `auth_test.go` | ✅ |
| T6 | Mesma aposta 50× em paralelo → um débito | `TestSameBetFiftyTimesInParallelDebitsOnce`, `TestIdenticalBetAcrossInstancesDebitsOnce` (3 processos) | ✅ |
| T7 | Duas apostas de 80.00 sobre 100.00 | `TestTwoConcurrentBetsOfEightyOnHundred`, `TestCompetingBetsAcrossInstancesNeverOverdraw` (3 processos) | ✅ |
| T8 | Carteiras distintas em paralelo | `TestSameWalletSerializesAndDistinctWalletsProceedInParallel`, `TestManyWalletsUnderConcurrentLoadStayConsistent` (3 processos) | ✅ |
| T9 | Três instâncias independentes | `test/e2e` (processos do SO), `docker-compose.yml` (`app-1..3`), `TestComposeInstancesShareState` | ✅ |
| T10 | Consumer interrompido após commit e antes do delete | `TestCrashAfterCommitBeforeDeleteIsRedeliveredSafely`, `TestConsumerCrashAfterCommitBeforeDeleteIsRedeliveredWithoutDoubleDebit` (`FAULT_CRASH_POINT`) | ✅ |
| T11 | Dois publishers disputando a outbox | `TestConcurrentPublishersPublishEachEventOnce`, `TestPublisherCrashBetweenPublishAndConfirmRepublishesSameEventID` (crash real + 2 processos) | ✅ |
| T12 | Reversão antes da referência: resolução e expiração | `TestReversalArrivingBeforeReferenceIsResolvedLater`, `TestPendingReferenceExpiresWithRejection`, `TestPendingReferenceSurvivesKillAndIsResolvedByAnotherInstance`, `TestPendingReferenceExpiresWhenReferenceNeverArrives` | ✅ |
| T13 | Reinício preserva idempotência, pendências e consistência | `TestReplayAfterRestartUsesPersistedState`, `TestKillDuringConcurrentLoadWithClientRetries`, `TestGracefulShutdownCompletesAndRestartPreservesIdempotency`, `TestPendingReferenceSurvivesKillAndIsResolvedByAnotherInstance` | ✅ |
| T14 | Cenários cruzando HTTP e SQS | `TestConcurrentHTTPAndSQSForTheSameOperation`, `TestSameOperationThroughHTTPAndSQSIsAppliedOnce` (processos reais) | ✅ |
| T15 | Reconciliação final saldo × ledger | `apptest.AssertAllWalletsReconcile` (integração) e `assertFinancialConsistency` ao final de todo cenário e2e | ✅ |
| T16 | Teste de carga k6 (opcional) | `test/load/wagering.js`, `make load`, [LOAD_TEST.md](LOAD_TEST.md) | ✅ |

## Entrega (§15)

| # | Item | Status |
|---|---|---|
| X1 | `README.md`: pré-requisitos, variáveis, filas, migrations, execução, exemplos de chamadas e testes | ✅ |
| X2 | `ARCHITECTURE.md` com dinheiro, transações, idempotência, locks, pendências, reversões, inbox/outbox, auth, Fx, shutdown, limitações e trabalho não concluído | ✅ |
| X3 | `.env.example` com valores locais, sem segredos reais | ✅ |
| X4 | `docker compose up --build`, `go test ./...`, `go test -race ./...`, `go vet ./...` | ✅ |
| X5 | Instruções separadas para dependências de teste, integração, multi-instância e falhas, com build tags | ✅ |
| X6 | Código formatado com `gofmt`, dependências reproduzíveis (`go.sum`, `go mod verify`) | ✅ |
| X7 | Provisionamento automático do IdP, identidades de teste e fluxos autenticados documentados | ✅ |
| X8 | Teste de carga com comando, ambiente, metodologia, throughput, p50/p95/p99, erros, conflitos e atraso da outbox | ✅ |
