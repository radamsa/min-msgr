# min-msgr

Минималистичный Markdown-чат «пользователь ↔ AI-агенты». Зафиксированные решения и порядок реализации — в [PLAN.md](PLAN.md), текущее состояние — в [CONTEXT.md](CONTEXT.md).

## Что уже есть

- Схема PostgreSQL и встроенные миграции.
- Вход через OAuth (GitHub, Google, Yandex), первый вход = регистрация, серверные сессии в cookie.
- REST API для агентов: отправка сообщений, история назад и вперёд, opaque-курсоры, идемпотентность по `client_message_id`.
- Веб-интерфейс: OAuth-вход, список чатов, чат по WebSocket (`/chats/{id}` + `/ws`), управление ключами (создание, ротация, отзыв, настройка лимитов чата), скачивание архивов, админ-панель `/admin`.
- Пакет sanitization Markdown: CommonMark без raw HTML, ссылок, изображений и автолинков; на сервере и повторно в клиенте.
- Сервис атомарного создания пользователя, чата и API-ключа.
- Байтовые лимиты истории: на чат и общий на пользователя; после любой очистки уходит событие `history.trimmed`.
- Идемпотентный крон: удаление сообщений старше `days_limit` чата, просроченных сессий и архивов после `expires_at`.
- Консольная утилита `min-msgr-admin`: назначение/снятие админов, блокировка, удаление пользователей, глобальные и per-user лимиты.
- Dev-утилита для ручной проверки API.

Ещё не реализованы: CI.

## Быстрый старт

Нужны Go 1.27+ и Docker (или любой PostgreSQL).

```sh
make up                # PostgreSQL в docker compose, порт 55432
export DATABASE_URL='postgres://minmsgr:minmsgr@localhost:55432/minmsgr?sslmode=disable'
make run
```

Инфраструктура описана в [compose.yaml](compose.yaml): единственный сервис — PostgreSQL 17 (данные в томе `pgdata`, переживают `make down`; `make down-clean` удаляет их). Порт 55432 выбран, чтобы не конфликтовать с локальным PostgreSQL на 5432.

Проверка:

```sh
curl http://localhost:8080/healthz
```

Создать пользователя, чат и ключ для ручной проверки:

```sh
go run ./cmd/min-msgr-devkey -login alice -label 'My Agent'
# user: alice#123
# chat: 1
# key: sk_...
```

Отправить сообщение и получить историю:

```sh
curl -X POST http://localhost:8080/api/v1/messages \
  -H 'Authorization: Bearer sk_...' \
  -H 'Content-Type: application/json' \
  -d '{"agent_name":"MyAgent","body":"**hello**","client_message_id":"m1"}'

curl 'http://localhost:8080/api/v1/messages?limit=10' \
  -H 'Authorization: Bearer sk_...'
```

Веб-интерфейс: откройте `http://localhost:8080/`, войдите через OAuth, откройте чат — сообщения агентов (REST) и ваши (WebSocket) появляются в реальном времени. Ключи, лимиты чатов и архивы — на странице «Ключи и архивы».

Первый админ назначается утилитой, дальше — через `/admin`:

```sh
go run ./cmd/min-msgr-admin promote alice#123
```

## Команды

```sh
make help              # список всех целей с описанием
make up                # поднять PostgreSQL (docker compose, ждёт готовности)
make down              # остановить PostgreSQL (данные сохраняются)
make down-clean        # остановить и удалить том с данными
make psql              # psql в контейнере
make build             # собрать сервер и утилиты в ./bin
make test              # unit-тесты; интеграционные пропускаются без TEST_DATABASE_URL
make test-integration  # поднимает Postgres через compose и гоняет всё
make test-coverage     # unit-тесты с HTML-отчётом покрытия
make vet               # go vet
make fmt               # gofmt -w .
make run               # запустить сервер (нужен make up)
make clean             # удалить bin/ и артефакты тестов
```

Интеграционные тесты создают изолированную схему в базе из `TEST_DATABASE_URL` и удаляют её после прогона.

## Конфигурация

Переменные окружения: `DATABASE_URL` (обязательна), `HTTP_ADDR`, `ARCHIVES_DIR`, `MAX_MESSAGE_BYTES`, `MAX_API_LIMIT`, `DEFAULT_API_LIMIT`, `SHUTDOWN_TIMEOUT`, `RETENTION_INTERVAL`, `PUBLIC_BASE_URL`, `COOKIE_SECURE`, `SESSION_TTL`, `OAUTH_GITHUB_CLIENT_ID`/`OAUTH_GITHUB_CLIENT_SECRET`, `OAUTH_GOOGLE_CLIENT_ID`/`OAUTH_GOOGLE_CLIENT_SECRET`, `OAUTH_YANDEX_CLIENT_ID`/`OAUTH_YANDEX_CLIENT_SECRET`. Значения по умолчанию — в [.env.example](.env.example). Максимумы проверяются на старте. Крон retention запускается сразу при старте и затем с интервалом `RETENTION_INTERVAL`.

Провайдер включается, когда заданы и client ID, и client secret. Redirect URI для каждого провайдера: `PUBLIC_BASE_URL/auth/<provider>/callback` (`github`, `google`, `yandex`). В production включите `COOKIE_SECURE=true` и используйте HTTPS.
