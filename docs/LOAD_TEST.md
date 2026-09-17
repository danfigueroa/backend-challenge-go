# Teste de carga

Script k6 em [`test/load/wagering.js`](../test/load/wagering.js), executado contra as três instâncias do Docker Compose. O objetivo não é só medir latência: toda execução termina reconciliando todas as carteiras usadas e falha se houver qualquer divergência entre saldo e ledger ou qualquer resposta fora do contrato.

## Como executar

```sh
OTEL_TRACES_SAMPLE_PERCENT=10 docker compose up --build -d --wait
make load            # perfil base (padrão)
make load-stress     # perfil acima da capacidade local
```

O k6 roda como serviço `k6` (perfil `load` do compose, imagem `grafana/k6:2.2.0`) dentro da rede do compose, então não precisa de instalação local. O resumo é exportado em `test/load/results/summary.json` (ignorado pelo git). O dashboard **Wallet Service** do Grafana (`http://localhost:3000`) mostra a execução em tempo real.

| Variável | Padrão | Efeito |
|---|---|---|
| `LOAD_RATE` | `50` | iterações/s do cenário `settlement`; `duplicates` usa `LOAD_RATE / 5` |
| `LOAD_HOT_VUS` | `10` | usuários virtuais em laço fechado sobre 5 carteiras concorridas |
| `LOAD_DURATION` | `2m` | duração dos cenários |
| `LOAD_WALLETS` | `200` | carteiras abertas no setup (mais 5 concorridas) |

## Cenários

| Cenário | Executor | O que faz | Verificações |
|---|---|---|---|
| `settlement` | taxa constante | `BET` em carteira aleatória, seguida de `WIN` (45%), `LOSS` (45%), `REFUND` (5%) ou `ROLLBACK` (5%) referenciando a aposta | aposta 200/422; liquidação 200 |
| `duplicates` | taxa constante | `BET` e, em seguida, a mesma requisição enviada **simultaneamente às três instâncias**; depois o mesmo `Idempotency-Key` com valor alterado | replays com `idempotentReplay: true` e o **mesmo saldo** da primeira resposta; alteração rejeitada com 409 |
| `hotWallets` | VUs constantes | apostas em laço fechado sobre 5 carteiras | contenção de lock na mesma carteira sem saldo negativo nem erro |

**Setup**: esvazia a fila de auditoria do LocalStack (sem consumidor, ela cresceria indefinidamente e degradaria o emulador), obtém tokens no Keycloak e abre as carteiras distribuindo as chamadas entre as instâncias.

**Teardown**: consulta o Prometheus até a outbox esvaziar (máximo de 8 minutos), registra o maior atraso observado e chama `POST /wallets/{id}/reconciliation` para **todas** as carteiras.

**Thresholds** (a execução falha se qualquer um for violado):

| Métrica | Limite |
|---|---|
| `http_req_duration{operation:bet}` e `{operation:settle}` | p95 < 250 ms, p99 < 500 ms |
| `http_req_duration{operation:hot_bet}` | p95 < 500 ms |
| `http_req_duration{operation:replay}` | p95 < 150 ms |
| `checks` | > 99,9% |
| `wallet_unexpected_responses` (status fora de 200/202/409/422/503) | 0 |
| `wallet_reconciliation_divergences` | 0 |

As respostas 409 do cenário `duplicates` são esperadas e aparecem em `http_req_failed`.

## Ambiente de medição

Todos os componentes na mesma máquina, o que significa que PostgreSQL, LocalStack, Keycloak, Jaeger, Prometheus, Grafana, k6 e as três instâncias disputam CPU e memória:

| Item | Valor |
|---|---|
| Host | MacBook, 8 núcleos, 8 GB de RAM |
| Docker Desktop | VM com 8 vCPUs e 4 GB |
| PostgreSQL | 18.6 com configuração padrão (`shared_buffers` 128 MB, `synchronous_commit = on`) |
| Instâncias | 3, todas com os papéis `api,consumer,outbox,pendingref`, `DATABASE_MAX_CONNS=20` |
| Tracing | amostragem de 10% (`OTEL_TRACES_SAMPLE_PERCENT=10`) |
| Protocolo | `docker compose down -v` e stack nova antes de cada perfil |

Os números abaixo descrevem **este** ambiente; servem para comparar versões e demonstrar o comportamento sob saturação, não como capacidade de produção.

## Resultados

### Perfil base (`make load`)

Todos os thresholds aprovados.

| Indicador | Valor |
|---|---|
| Requisições HTTP em 2 min | 95.080 (~790 req/s) |
| Transações processadas | 89.621 (pico de 814 tx/s) |
| Replays idempotentes | 3.603 |
| Conflitos 409 intencionais | 1.201 |
| Respostas 5xx / fora do contrato | 0 / 0 |
| Checks | 96.827 de 96.827 (100%) |
| Reconciliação | 205 carteiras, 87.123 lançamentos, **0 divergências** |

| Operação | p50 | p95 | p99 |
|---|---|---|---|
| `BET` | 10,1 ms | 57,2 ms | 158,0 ms |
| Liquidação (`WIN`/`LOSS`) | 8,9 ms | 42,9 ms | 110,3 ms |
| `BET` em carteira concorrida | 10,9 ms | 37,6 ms | 76,5 ms |
| Replay | 1,6 ms | 8,0 ms | 21,9 ms |
| Processamento no servidor (histograma da aplicação) | — | 42,5 ms | 91,8 ms |

| Outbox | Valor |
|---|---|
| Pico de eventos pendentes | 150.090 |
| Pico de publicação | 751 eventos/s |
| Latência de publicação no SNS (p50 / p95) | 20 ms / 82 ms |
| Tempo para esvaziar após a carga | 241 s |
| Falhas transitórias do SNS / eventos adiados para manter a ordem | 15 / 99 |

| Aplicação (por instância) | Valor |
|---|---|
| Memória residente | ~44 MB |
| Goroutines | ~200 |
| CPU das três instâncias somadas (pico) | 1,5 núcleo |

### Perfil de estresse (`make load-stress`)

`LOAD_RATE=200` e `LOAD_HOT_VUS=40`: cerca de quatro vezes a carga do perfil base.

| Indicador | Valor |
|---|---|
| Requisições HTTP em 2 min | 96.850 |
| Transações processadas | 77.348 (pico de 755 tx/s) |
| Replays idempotentes / conflitos 409 | 14.184 / 4.728 |
| Iterações descartadas pelo k6 | 767 |
| Respostas 5xx / fora do contrato | **0 / 0** |
| Reconciliação | 205 carteiras, 67.011 lançamentos, **0 divergências** |

| Operação | p50 | p95 | p99 |
|---|---|---|---|
| `BET` | 95,0 ms | 828,9 ms | 1,35 s |
| Liquidação | 80,9 ms | 780,4 ms | 1,34 s |
| `BET` em carteira concorrida | 86,5 ms | 711,7 ms | 1,20 s |
| Replay | 17,7 ms | 252,2 ms | 453,9 ms |

A vazão estabiliza em torno de 750–800 tx/s, o mesmo patamar do pico do perfil base: acima disso o excedente vira fila e latência (thresholds de latência violados, como esperado), mas **nenhuma requisição falha, nenhuma resposta sai do contrato e nenhuma carteira diverge**. A degradação é graciosa.

## Gargalos encontrados e correções

A primeira execução completa do perfil de estresse expôs dois problemas reais.

### 1. Varredura sequencial do ledger a cada commit

**Sintoma**: latência crescendo ao longo do teste até p95 de 10 s, 763 respostas 503 por timeout de requisição e 12.880 iterações descartadas, com o PostgreSQL consumindo CPU (medição ainda com tracing a 100% e estado acumulado, ver item 3).

**Diagnóstico**: `pg_stat_user_tables` mostrou 34.662 varreduras sequenciais em `ledger_entries`, com 572 milhões de tuplas lidas. A origem era o constraint trigger adiado `wager_transactions_check_ledger`, que verificava a existência do lançamento com `WHERE transaction_id = ...`. O único índice que contém `transaction_id` é `UNIQUE (wallet_id, transaction_id)`, cuja primeira coluna não estava no filtro, então cada commit percorria o ledger inteiro: custo O(n) por transação, crescente com o histórico.

**Correção**: migration `000007_scope_transaction_ledger_check` restringe a busca por `wallet_id AND transaction_id`, usando o índice único existente (sem índice novo, sem custo de escrita adicional). Após a correção, o mesmo teste registrou **0 varreduras sequenciais** em todas as tabelas.

**Regressão**: `TestIntegrityTriggersReadLedgerByIndexedLookups` cria 300 carteiras com histórico, grava duas transações em uma conexão isolada e mede as tuplas lidas de `ledger_entries` pelas estatísticas do PostgreSQL. Sem a correção, lê 1.209 tuplas; com a correção, 6.

### 2. Publicação sequencial da outbox

**Sintoma**: atraso da outbox crescendo continuamente sob carga.

**Correção**: o publisher passou a agrupar cada lote por `partitionKey` (carteira) e publicar os grupos em paralelo (`OUTBOX_PUBLISH_CONCURRENCY`, padrão 8), mantendo a ordem dentro de cada carteira. Se um evento falha, os seguintes da mesma carteira no lote são adiados para o mesmo instante do retry, preservando a ordem. Detalhes em [ARCHITECTURE.md › Publicação com transactional outbox](../ARCHITECTURE.md#publicação-com-transactional-outbox).

**Limite restante**: com a publicação paralela, o gargalo passou a ser o **LocalStack**. A latência de publicação sobe de ~3 ms ocioso para 20–80 ms sob carga, com o contêiner do emulador (processo Python) preso em ~100% de um núcleo, e a vazão fica em ~750 eventos/s. Como cada transação gera dois eventos, a outbox acumula backlog durante a carga e o esvazia depois, sem perda nem duplicidade. Em AWS real, o SNS FIFO suporta vazão muito maior por grupo de mensagens; se necessário, a vazão da outbox escala com mais instâncias no papel `outbox`, com `OUTBOX_PUBLISH_CONCURRENCY` ou com `PublishBatch` (até 10 mensagens por chamada).

### 3. Condições do ambiente

- **Fila de auditoria sem consumidor**: acumulou 167 mil mensagens e deixou o LocalStack lento para todas as operações. O setup do k6 agora esvazia a fila antes de cada execução.
- **Tracing a 100%**: o Jaeger em memória chegou a quase 1 GB dentro de uma VM de 4 GB. Para carga, use `OTEL_TRACES_SAMPLE_PERCENT=10`.
- **Estado acumulado**: execuções seguidas sobre o mesmo volume (centenas de MB na outbox, já que a retenção de eventos publicados está fora do escopo) produziram resultados inconsistentes. Por isso o protocolo recria a stack antes de cada perfil.
