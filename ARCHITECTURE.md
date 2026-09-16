# Arquitetura

Registro das decisões técnicas da solução. Cada seção é preenchida junto com a implementação correspondente; o progresso por requisito está em [`docs/REQUIREMENTS.md`](docs/REQUIREMENTS.md).

## Visão geral

Arquitetura hexagonal (ports & adapters):

```
cmd/wallet/          binário único (API, workers, subcomando migrate)
internal/domain/     modelo puro: money, wallet, wagering, event
internal/app/        casos de uso e ports (interfaces)
internal/adapter/    postgres, http, auth, aws, sqsconsumer
internal/worker/     outbox, pendingref, runner
internal/platform/   config, logger, metrics, tracing, health, faultinject
internal/fxapp/      módulos Fx e seleção de roles
```

Regra de dependência: `domain` ← `app` ← `adapter`/`worker` ← `fxapp` ← `cmd`. O domínio não importa Fx, HTTP, SQS nem bibliotecas de persistência.

## Decisões

| Tema | Decisão | Seção |
|---|---|---|
| Go | 1.27.1 em `go.mod` e `golang:1.27.1-alpine` no Dockerfile | — |
| Dinheiro | `int64` em unidades mínimas, `BIGINT` no Postgres, formato estrito | [Dinheiro](#dinheiro) |
| Concorrência | Lock pessimista por carteira + versão condicionada + constraints | [Concorrência](#concorrência) |
| Banco | `pgx/v5` com SQL explícito, `golang-migrate` | [Transações SQL](#transações-sql) |
| Mensageria local | LocalStack `4.14.0` community | [Mensageria](#mensageria) |
| Reversões | Cada transação revertida no máximo uma vez | [Reversões](#reversões) |
| Referências pendentes | TTL (padrão 30 min) + backoff exponencial com jitter; `WIN` com referência também aguarda | [Referências](#referências) |
| Validação de carteira | Carteira inexistente/de outro jogador/outra moeda é erro corrigível, não persistido | [Serviço de domínio](#serviço-de-domínio-process) |
| Identificadores externos | ASCII visível sem normalização; UUIDs canônicos minúsculos | [Requisição externa](#requisição-externa-request) |

## Dinheiro

Pacote `internal/domain/money`.

### Representação

`Money` é um value object imutável com dois campos não exportados: `minor int64` (unidades mínimas, centavos) e `currency Currency`. Todas as moedas suportadas usam escala fixa `Scale = 2`.

| Aspecto | Decisão |
|---|---|
| Tipo interno | `int64` em unidades mínimas |
| Faixa representável | `-92233720368547758.08` a `92233720368547758.07` |
| Moedas suportadas | `BRL`, `USD`, `EUR` (ISO 4217, todas com 2 casas) |
| Persistência | `BIGINT` (unidades mínimas) + `CHAR(3)` (código da moeda) |
| Contrato externo | `{"amount":"25.00","currency":"BRL"}` |
| Ponto flutuante | Proibido: nenhuma função usa `float32`/`float64`; o `golangci-lint` (`forbidigo`) bloqueia os tipos e `strconv.ParseFloat` fora de métricas |

`Currency` é uma struct com campo não exportado: só pode ser obtida pelas variáveis `money.BRL/USD/EUR` ou por `ParseCurrency`, o que impede moedas arbitrárias. O valor zero de `Money` e de `Currency` é inválido e rejeitado por aritmética, comparação e serialização (`ErrUninitialized`).

### Parsing estrito

Gramática aceita: `("0" | [1-9][0-9]*) "." [0-9]{2}`.

- Rejeitados com `ErrInvalidAmount`: vazio, `25`, `25.0`, `25.000`, `.50`, `025.00`, `+25.00`, `2.5e1`, `NaN`, `Infinity`, separadores (`1,000.00`), espaços, dígitos não ASCII e números JSON (`"amount": 25.00`).
- Rejeitados com `ErrNegativeAmount`: qualquer valor com sinal em `Parse` (entrada externa).
- Rejeitados com `ErrOverflow`: valores acima do limite de `int64`.
- Moeda precisa ser exatamente um código suportado em maiúsculas (`brl` é rejeitado com `ErrInvalidCurrency`).

**Não há normalização nem arredondamento**: cada valor aceito tem uma única forma textual, e `Amount()` devolve exatamente a string recebida. Por isso o hash de idempotência usa a string original sem transformação.

O parsing valida todo o formato antes de acumular dígitos (assim uma entrada malformada nunca é classificada como overflow) e acumula no lado negativo de `int64`, o que cobre `math.MinInt64` sem conversões `uint64 → int64`.

`ParseSigned` aceita sinal negativo e existe apenas para valores internos (por exemplo, `difference` da reconciliação); adaptadores de entrada usam `Parse`.

### Operações

`Add`, `Sub`, `Neg` e `Cmp` retornam erro em vez de causar panic:

- `ErrCurrencyMismatch` para moedas diferentes;
- `ErrOverflow` quando o resultado sai da faixa de `int64` (inclusive `-MinInt64`);
- `ErrUninitialized` para operandos no valor zero.

`Equal` não falha: moedas diferentes simplesmente não são iguais. Valores negativos são permitidos em cálculos internos; a não negatividade do saldo é responsabilidade do agregado `Wallet` e de `CHECK` no banco.

### Erros

Sentinelas classificáveis com `errors.Is`: `ErrInvalidAmount`, `ErrNegativeAmount`, `ErrInvalidCurrency`, `ErrCurrencyMismatch`, `ErrOverflow`, `ErrUninitialized`.

### Testes

- Tabelas cobrindo entradas válidas, inválidas, limites de `int64`, overflow em parsing/soma/subtração/negação, moedas incompatíveis, valor não inicializado e JSON.
- `FuzzParse` compara o parser com um oráculo independente (regex + `math/big`): toda entrada aceita pertence à gramática, cabe em `int64`, tem o valor exato e volta à mesma string; toda rejeição é classificada. O corpus em `testdata/fuzz` preserva uma regressão encontrada pelo fuzzer (entrada malformada classificada como overflow).

## Carteira e ledger

Pacote `internal/domain/wallet`.

### Agregado `Wallet`

Raiz do agregado financeiro, com estado não exportado: `id`, `playerID`, `balance` (`Money`, que carrega a moeda), `version`, `loadedVersion`, `createdAt` e `updatedAt` (sempre em UTC).

| Operação | Comportamento |
|---|---|
| `Open` | Cria a carteira com versão `1`. Saldo inicial positivo gera o lançamento de crédito da abertura (`0.00 → saldo`, `walletVersion = 1`) vinculado ao `OPENING`; saldo zero não gera lançamento. Saldo negativo é rejeitado. |
| `Rehydrate` | Reconstrói a partir do banco **sem** gerar lançamentos nem alterar versão. Valida as invariantes (saldo não negativo, versão ≥ 1, timestamps coerentes) para detectar dados corrompidos. |
| `Debit` / `Credit` | Exigem valor positivo e mesma moeda. Débito exige saldo suficiente (`ErrInsufficientFunds`). Cada movimentação devolve o `LedgerEntry` correspondente, incrementa a versão em 1 e só avança `updatedAt`. |
| `CanDebit` | Consulta de saldo suficiente sem alterar estado. |

Qualquer rejeição deixa o agregado intacto: o lançamento é construído e validado **antes** de alterar saldo e versão.

`loadedVersion` guarda a versão lida do banco. O repositório a usa no `UPDATE … WHERE version = $loadedVersion`, que detecta escritas concorrentes mesmo que o lock de linha falhe (ver [Concorrência](#concorrência)). `IsNew()` e `HasChanges()` informam se a carteira precisa de `INSERT`, `UPDATE` ou nenhuma escrita (por exemplo, `LOSS`).

A unicidade de `(playerId, currency)` não cabe no agregado (exige visão de todas as carteiras) e é garantida por `UNIQUE` no banco.

### `LedgerEntry`

Lançamento imutável (campos não exportados, somente getters): `id`, `walletId`, `transactionId`, `direction` (`DEBIT`/`CREDIT`), `amount`, `balanceBefore`, `balanceAfter`, `walletVersion` e `createdAt`.

A construção valida:

- identidades não nulas, `walletVersion ≥ 1` e timestamp presente;
- `amount > 0`, saldos não negativos e mesma moeda nos três valores;
- `balanceAfter = balanceBefore − amount` (débito) ou `balanceBefore + amount` (crédito), com aritmética checada contra overflow.

Lançamentos só são criados pelo agregado (`Open`, `Debit`, `Credit`). `RehydrateLedgerEntry` aplica as mesmas validações ao ler do banco.

`walletVersion` é a versão da carteira **após** o lançamento. Como toda mudança de saldo incrementa a versão exatamente uma vez, `(walletId, walletVersion)` é único e sequencial: serve como cursor estável da paginação e como prova, no banco, de que nenhuma atualização foi perdida.

### Erros

`ErrInvalidWallet`, `ErrInvalidLedgerEntry`, `ErrInvalidAmount`, `ErrCurrencyMismatch`, `ErrInsufficientFunds`, `ErrBalanceOverflow`, todos classificáveis com `errors.Is`.

## Transações de aposta

Pacote `internal/domain/wagering`.

### Requisição externa (`Request`)

HTTP e SQS convertem seus corpos em `RequestInput` (somente strings) e chamam `wagering.NewRequest`, que é o **único** ponto de validação de entrada. Assim as duas portas aplicam exatamente as mesmas regras e produzem o mesmo hash.

| Campo | Regra |
|---|---|
| `providerId`, `externalTransactionId`, `roundId`, `gameId` | Obrigatórios, 1–128 bytes, apenas ASCII visível (`!`–`~`). Sem trim: espaços são rejeitados, não removidos. |
| `idempotencyKey` | Obrigatório, 1–255 bytes, ASCII visível. |
| `playerId`, `walletId` | UUID na forma canônica minúscula com hífens (`uuid.String()`); maiúsculas, chaves, `urn:` e UUID nulo são rejeitados. |
| `kind` | `BET`, `WIN`, `LOSS`, `REFUND` ou `ROLLBACK`. `OPENING` é rejeitado com `OPENING_NOT_ALLOWED`. |
| `money` | `money.Parse` estrito (ver [Dinheiro](#dinheiro)). |
| Valor zero | `LOSS` exige `0.00` (`LOSS_AMOUNT_MUST_BE_ZERO`); os demais exigem valor > 0 (`AMOUNT_MUST_BE_POSITIVE`). |
| `referenceExternalTransactionId` | Obrigatório em `REFUND`/`ROLLBACK`, opcional em `WIN`, proibido em `BET`/`LOSS`; não pode ser igual ao próprio `externalTransactionId`. |

Erros de validação são `*ValidationError{Code, Field, Reason}`, classificáveis por `errors.As` e por `errors.Is(err, ErrValidation)`.

### Hash de idempotência

Como a entrada é estrita, **não existe normalização**: toda requisição aceita tem uma única forma textual. O hash é `SHA-256` do JSON canônico abaixo, gravado como `BYTEA` (32 bytes) e exposto em hexadecimal minúsculo.

- Chaves ordenadas lexicograficamente, sem espaços, UTF-8, sem escape de HTML (compatível com RFC 8785 para strings ASCII).
- Campos: `externalTransactionId`, `gameId`, `kind`, `money.amount`, `money.currency`, `playerId`, `providerId`, `referenceExternalTransactionId` (omitido quando ausente), `roundId`, `walletId`.
- Excluídos: `idempotencyKey`, `messageId`, `type`, `occurredAt`, headers e qualquer metadado de transporte.

Exemplo (golden test `TestCanonicalPayloadGolden`):

```json
{"externalTransactionId":"transaction-123","gameId":"fortune-chimp","kind":"BET","money":{"amount":"25.00","currency":"BRL"},"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","providerId":"provider-a","roundId":"round-987","walletId":"0192f291-27dd-7d3f-8071-5f8685deef37"}
```

`sha256 = 629836932b79106b99523d06a1e7fa80689b0ea1e1c47aa3f0a5a2c87d0c4344`

O JSON é montado manualmente (sem reflexão) e um teste compara o resultado com `encoding/json` sobre um mapa ordenado para inputs com `"`, `\` e `<&>`, garantindo equivalência byte a byte.

### Entidade `Transaction`

Estado não exportado; construtores distintos por origem:

| Construtor | Uso |
|---|---|
| `NewExternal(id, Request, now)` | Operação de provedor; inicia em `PENDING` com todos os metadados externos e o hash. |
| `NewOpening(OpeningParams)` | Abertura interna; `origin = INTERNAL`, `kind = OPENING`, sem provedor, IDs externos, chave, hash, rodada, jogo ou referência. Valor precisa ser positivo. |
| `Rehydrate(RehydrateParams)` | Leitura do banco: valida a consistência completa (origem × metadados, tipo × valor, estado × dados de conclusão) sem executar transições nem gerar eventos. |

Transições explícitas: `MarkProcessed`, `Reject`, `Fail`, `AwaitReference`. Cada uma valida o estado de origem e os dados exigidos pelo estado de destino.

### Máquina de estados

```mermaid
stateDiagram-v2
    [*] --> PENDING
    PENDING --> PROCESSED: MarkProcessed
    PENDING --> REJECTED: Reject (código DEFINITIVE)
    PENDING --> FAILED: Fail (código INFRASTRUCTURE)
    PENDING --> PENDING_REFERENCE: AwaitReference
    PENDING_REFERENCE --> PENDING_REFERENCE: AwaitReference (novo retry)
    PENDING_REFERENCE --> PROCESSED
    PENDING_REFERENCE --> REJECTED
    PENDING_REFERENCE --> FAILED
    PROCESSED --> [*]
    REJECTED --> [*]
    FAILED --> [*]
```

- Estados terminais (`PROCESSED`, `REJECTED`, `FAILED`) recusam qualquer transição com `ErrInvalidTransition`, sem alterar o estado.
- `PROCESSED` exige resultado (saldo e versão da carteira após o processamento) e, se houver referência, o ID interno resolvido.
- `REJECTED` só aceita códigos de categoria `DEFINITIVE` e registra o saldo observado no momento da rejeição.
- `FAILED` só aceita códigos de categoria `INFRASTRUCTURE`.
- `PENDING_REFERENCE` exige `nextAttemptAt` e `expiresAt`; `expiresAt` é fixado na primeira espera e nunca muda; cada nova espera incrementa `attempts`.

### Serviço de domínio `Process`

`wagering.Process` concentra a regra de negócio e é usado tanto pelo processamento síncrono (HTTP/SQS) quanto pelo worker de pendências. Recebe a transação, a carteira **já bloqueada**, a referência resolvida (se houver), o ID do lançamento, o instante atual e a política de espera, e devolve uma `Decision` (`PROCESSED`, `REJECTED` ou `AWAITING_REFERENCE`) com o lançamento criado, quando existir. A camada de aplicação apenas carrega, persiste e publica.

| Tipo | Movimento | Código em saldo insuficiente |
|---|---|---|
| `BET` | Débito | `INSUFFICIENT_FUNDS` |
| `WIN` | Crédito | — |
| `LOSS` | Nenhum (sem lançamento, sem nova versão) | — |
| `REFUND` | Crédito do valor da `BET` | — |
| `ROLLBACK` de `BET` | Crédito | — |
| `ROLLBACK` de `WIN`/`REFUND` | Débito | `REVERSAL_INSUFFICIENT_FUNDS` |

Crédito que ultrapassaria o limite de `int64` é rejeitado com `BALANCE_LIMIT_EXCEEDED`.

Antes de processar, `CheckWallet` confere se a carteira pertence ao jogador (`WALLET_PLAYER_MISMATCH`) e usa a moeda da operação (`WALLET_CURRENCY_MISMATCH`). São erros corrigíveis e **não** são persistidos, assim como `WALLET_NOT_FOUND`.

## Referências

### Resolução

A referência é buscada por `(providerId, referenceExternalTransactionId)` e avaliada nesta ordem:

| Situação da referência | Resultado |
|---|---|
| Não encontrada | Aguarda (`PENDING_REFERENCE`, motivo `REFERENCE_NOT_FOUND`) |
| `PENDING` ou `PENDING_REFERENCE` | Aguarda (motivo `REFERENCE_NOT_PROCESSED`); cadeias fora de ordem se resolvem sozinhas |
| `REJECTED` ou `FAILED` | Rejeita `REFERENCE_NOT_PROCESSED` |
| Tipo incompatível (`REFUND`→não `BET`; `ROLLBACK`→não `BET`/`WIN`/`REFUND`; `WIN`→não `BET`; interna) | Rejeita `REFERENCE_KIND_INVALID` |
| Provedor, jogador, carteira, moeda ou rodada divergentes | Rejeita `REFERENCE_PROVIDER_MISMATCH`, `REFERENCE_PLAYER_MISMATCH`, `REFERENCE_WALLET_MISMATCH`, `REFERENCE_CURRENCY_MISMATCH`, `REFERENCE_ROUND_MISMATCH` |
| Valor diferente (somente reversões; parciais não são suportadas) | Rejeita `REFERENCE_AMOUNT_MISMATCH` |
| Já revertida (somente reversões) | Rejeita `REFERENCE_ALREADY_REVERSED` |

`WIN` com referência segue o mesmo fluxo de espera: só credita depois de validar a `BET` da mesma rodada.

### Espera e expiração

Política (`PendingPolicy`), configurável por ambiente:

| Parâmetro | Padrão | Significado |
|---|---|---|
| `TTL` | 30 min | Prazo contado a partir do recebimento (`createdAt`) |
| `BaseDelay` | 1 s | Primeiro intervalo de retry |
| `MaxDelay` | 60 s | Teto do backoff exponencial |
| `Jitter` | aleatório | Soma um valor não negativo ao intervalo para evitar sincronização entre instâncias |

O intervalo da tentativa `n` é `min(BaseDelay × 2^(n−1), MaxDelay) + jitter`, limitado a `expiresAt`. Quando uma avaliação ocorre em `now ≥ expiresAt`, a transação é finalizada como `REJECTED` com o motivo da espera (`REFERENCE_NOT_FOUND` se nunca chegou; `REFERENCE_NOT_PROCESSED` se chegou mas não concluiu), gerando `WagerTransactionRejected`. Optou-se por TTL em vez de número máximo de tentativas para dar ao provedor um prazo previsível, independente do backoff e de indisponibilidades.

## Reversões

Regra adotada: **cada transação pode ser revertida com sucesso no máximo uma vez**, por `REFUND` ou por `ROLLBACK`. No banco, isso será garantido por um índice único parcial em `reference_transaction_id` para `REFUND`/`ROLLBACK` em `PROCESSED`; no domínio, pela flag `AlreadyReversed` informada pelo repositório (com a linha da referência bloqueada).

| Sequência sobre a mesma `BET` | Resultado |
|---|---|
| `REFUND` → `REFUND` | Segundo rejeitado (`REFERENCE_ALREADY_REVERSED`) |
| `REFUND` → `ROLLBACK` da `BET` | Rejeitado (`REFERENCE_ALREADY_REVERSED`); impede devolução dupla |
| `ROLLBACK` → `REFUND` da `BET` | Rejeitado (`REFERENCE_ALREADY_REVERSED`) |
| `REFUND` → `ROLLBACK` do `REFUND` | Permitido: debita o valor devolvido (a `BET` volta a estar cobrada) |
| `ROLLBACK` do `REFUND` → novo `REFUND` da `BET` | Rejeitado: a `BET` continua marcada como revertida pelo `REFUND` original, que permanece `PROCESSED` |

Assim, o mesmo débito nunca é devolvido duas vezes e o histórico permanece auditável, sem estados "reabertos".

## Códigos de falha

`failureCode` estáveis, com categoria que indica se o cliente pode corrigir e reenviar:

| Categoria | Persistido? | Códigos |
|---|---|---|
| `CORRECTABLE` | Não (a chave de idempotência não é consumida) | `MALFORMED_REQUEST`, `MISSING_FIELD`, `INVALID_FIELD`, `INVALID_AMOUNT`, `INVALID_CURRENCY`, `UNSUPPORTED_KIND`, `OPENING_NOT_ALLOWED`, `AMOUNT_MUST_BE_POSITIVE`, `LOSS_AMOUNT_MUST_BE_ZERO`, `REFERENCE_REQUIRED`, `REFERENCE_NOT_ALLOWED`, `SELF_REFERENCE`, `WALLET_NOT_FOUND`, `WALLET_PLAYER_MISMATCH`, `WALLET_CURRENCY_MISMATCH` |
| `DEFINITIVE` | Sim, `REJECTED` (terminal) | `INSUFFICIENT_FUNDS`, `REVERSAL_INSUFFICIENT_FUNDS`, `BALANCE_LIMIT_EXCEEDED`, `REFERENCE_NOT_FOUND`, `REFERENCE_NOT_PROCESSED`, `REFERENCE_ALREADY_REVERSED`, `REFERENCE_KIND_INVALID`, `REFERENCE_PROVIDER_MISMATCH`, `REFERENCE_PLAYER_MISMATCH`, `REFERENCE_WALLET_MISMATCH`, `REFERENCE_ROUND_MISMATCH`, `REFERENCE_CURRENCY_MISMATCH`, `REFERENCE_AMOUNT_MISMATCH` |
| `CONFLICT` | Não | `IDEMPOTENCY_KEY_CONFLICT`, `EXTERNAL_TRANSACTION_CONFLICT` |
| `INFRASTRUCTURE` | Sim, `FAILED` (terminal) | `INTERNAL_PROCESSING_FAILED` |

## Eventos de integração

Pacote `internal/domain/event`. Cada evento tem um tipo concreto (`Envelope[T]` com payload tipado), e **tipo e versão são definidos pelo construtor**, nunca pelo chamador. Construtores validam o estado de origem (por exemplo, `NewWagerTransactionProcessed` exige transação `PROCESSED`).

### Envelope

```json
{
  "eventId": "uuid",
  "eventType": "WalletBalanceChanged",
  "aggregateType": "Wallet",
  "aggregateId": "uuid",
  "correlationId": "string",
  "causationId": "string (opcional)",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "version": 1,
  "data": { }
}
```

Timestamps sempre em UTC, RFC 3339 com milissegundos; valores monetários no formato `{"amount":"25.00","currency":"BRL"}`. `PartitionKey` (o `walletId`) não faz parte do JSON: é usado como `MessageGroupId` na publicação, garantindo ordem por carteira.

| Evento | Agregado | Gatilho | Payload (`data`) |
|---|---|---|---|
| `WagerTransactionProcessed` | `WagerTransaction` | Operação concluída com sucesso, inclusive `LOSS` e `OPENING` | dados da transação, `referenceTransactionId?`, `balance`, `walletVersion`, `processedAt` |
| `WagerTransactionRejected` | `WagerTransaction` | Rejeição definitiva | dados da transação, `referenceTransactionId?`, `failureCode`, `failureCategory`, `rejectedAt` |
| `WagerTransactionPendingReference` | `WagerTransaction` | Registro/renovação da espera | dados da transação, `waitingReason`, `attempts`, `nextAttemptAt`, `expiresAt` |
| `WalletBalanceChanged` | `Wallet` | Mudança efetiva de saldo | `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter`, `walletVersion` |

"Dados da transação" = `transactionId`, `origin`, `kind`, `walletId`, `playerId`, `money` e, apenas para origem externa, `providerId`, `externalTransactionId`, `roundId`, `gameId`, `referenceExternalTransactionId`. Eventos de `OPENING` omitem esses metadados externos inaplicáveis. Os contratos são travados por testes golden de JSON.

## Transações SQL

_A preencher na Fase 2._

## Idempotência

_A preencher na Fase 3._

## Concorrência

_A preencher na Fase 3._

## Mensageria

### Escolha do emulador

As imagens `localstack/localstack` publicadas a partir de 2026 (`2026.x`, `latest`, `stable`) encerram na inicialização sem `LOCALSTACK_AUTH_TOKEN` (exit code 55, _License activation failed_). A tag `4.14.0` é a última edição community que roda sem credenciais. Foi validada localmente para os recursos usados:

- SQS FIFO com `RedrivePolicy` movendo a mensagem para a DLQ ao exceder `maxReceiveCount`;
- SNS FIFO (`wallet-events.fifo`) com assinatura `RawMessageDelivery` em fila SQS FIFO e deduplicação por `MessageDeduplicationId` (publicação repetida entregue uma única vez).

_Inbox, outbox e consumidor serão detalhados na Fase 6._

## Autenticação e autorização

_A preencher na Fase 5._

## Uso do Fx e shutdown

_A preencher na Fase 4._

## Observabilidade

_A preencher na Fase 4._

## Limitações e interpretações

- **Moedas**: apenas `BRL`, `USD` e `EUR`, todas com duas casas decimais. Moedas com outra escala (por exemplo, `JPY`) exigiriam escala por moeda.
- **Formatos equivalentes não são aceitos**: `25`, `25.0`, UUIDs em maiúsculas e identificadores com espaços são rejeitados em vez de normalizados, eliminando ambiguidade no hash de idempotência.
- **Reversões parciais** não são suportadas (conforme o enunciado).
- **Reversão única por transação**: um `ROLLBACK` de `REFUND` não reabre a `BET` para nova devolução.
