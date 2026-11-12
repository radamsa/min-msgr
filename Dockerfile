# Многоступенчатая сборка: builder компилирует статический бинарник, runtime —
# минимальный образ без тулчейна.
FROM golang:1.27.2-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown

# ca-certificates нужны на этапе сборки для `go mod download` через HTTPS.
RUN apk add --no-cache ca-certificates git

WORKDIR /src

# Сначала зависимости: слой кэшируется, пока go.mod/go.sum не изменились.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# Статический бинарник (CGO=0): pgx чисто Go, cgo не нужен.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
    -o /out/min-msgr ./cmd/min-msgr

# --- runtime ---
FROM alpine:3.21

# Сертификаты для OAuth-запросов к провайдерам, wget для healthcheck.
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S app && adduser -S -G app app

WORKDIR /app
COPY --from=builder /out/min-msgr /app/min-msgr

# Архивы чатов живут здесь; том монтируется в это место.
RUN mkdir -p /app/data/archives && chown -R app:app /app/data

USER app

EXPOSE 8080

HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=5 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/app/min-msgr"]
