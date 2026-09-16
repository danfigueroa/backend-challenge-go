# syntax=docker/dockerfile:1.7

FROM golang:1.27.1-alpine3.24 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations

ARG BUILD_TAGS=""
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -tags "${BUILD_TAGS}" \
      -ldflags "-s -w" \
      -o /out/wallet ./cmd/wallet

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/wallet /wallet
USER nonroot:nonroot
EXPOSE 8080 9090
HEALTHCHECK --interval=10s --timeout=3s --start-period=20s --retries=3 CMD ["/wallet", "healthcheck"]
ENTRYPOINT ["/wallet"]
CMD ["serve"]
