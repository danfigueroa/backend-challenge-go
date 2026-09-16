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

As seções de execução, variáveis de ambiente, migrations, filas, autenticação, exemplos de chamadas e testes de integração/falhas serão adicionadas à medida que cada componente for entregue.
