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

### Всё в Docker

```sh
make up         # соберёт образ и поднимет PostgreSQL + сервис
# приложение: http://localhost:8080
```

### Локально (go run)

```sh
docker compose up -d --wait postgres   # только БД, порт 55432
export DATABASE_URL='postgres://minmsgr:minmsgr@localhost:55432/minmsgr?sslmode=disable'
make run
```

Инфраструктура описана в [compose.yaml](compose.yaml): сервис `app` (HTTP + WebSocket + retention-крон, миграции применяются при старте) и PostgreSQL 17 (данные в томе `pgdata`; архивы — в томе `archives`). Порт 55432 наружу выбран, чтобы не конфликтовать с локальным PostgreSQL на 5432. Остановка с удалением томов: `make down-clean`.

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

Веб-интерфейс: откройте `http://localhost:8080/`, войдите через OAuth (см. [Регистрация и OAuth](#регистрация-и-oauth)), откройте чат — сообщения агентов (REST) и ваши (WebSocket) появляются в реальном времени. Ключи, лимиты чатов и архивы — на странице «Ключи и архивы».

Первый админ назначается утилитой, дальше — через `/admin`:

```sh
go run ./cmd/min-msgr-admin promote alice#123
```

## Команды

```sh
make help              # список всех целей с описанием
make up                # собрать образ и поднять весь стек (PostgreSQL + сервис)
make down              # остановить стек (данные сохраняются)
make down-clean        # остановить и удалить тома с данными
make psql              # psql в контейнере
make build             # собрать сервер и утилиты в ./bin
make test              # unit-тесты; интеграционные пропускаются без TEST_DATABASE_URL
make test-integration  # поднимает Postgres через compose и гоняет всё
make test-coverage     # unit-тесты с HTML-отчётом покрытия
make vet               # go vet
make fmt               # gofmt -w .
make run               # запустить сервер локально (нужен make up-db)
make clean             # удалить bin/ и артефакты тестов
```

Интеграционные тесты создают изолированную схему в базе из `TEST_DATABASE_URL` и удаляют её после прогона.

## Регистрация и OAuth

### Как работает вход

- Вход возможен **только** через OAuth: GitHub, Google или Yandex. Отдельной регистрации с паролем нет.
- **Первый успешный вход = регистрация**: пользователь создаётся автоматически с публичным ID вида `<oauth_login>#<3 цифры>` (например, `alice#417` — дискриминатор делает одинаковые логины уникальными).
- Повторный вход того же OAuth-аккаунта открывает существующего пользователя — аккаунты не дублируются.
- Без настроенных провайдеров сервис работает, но страница `/login` покажет «OAuth-провайдеры не настроены», и войти в веб нельзя (агентский REST API при этом продолжает работать по ключам).

### Настройка провайдеров

Провайдер включается, когда заданы **и client ID, и client secret**; частичная конфигурация — ошибка старта. Redirect URI каждого провайдера жёстко привязан к `PUBLIC_BASE_URL`:

```
<PUBLIC_BASE_URL>/auth/github/callback
<PUBLIC_BASE_URL>/auth/google/callback
<PUBLIC_BASE_URL>/auth/yandex/callback
```

Для локального запуска по умолчанию `PUBLIC_BASE_URL=http://localhost:8080`.

**GitHub**: [github.com/settings/developers](https://github.com/settings/developers) → «New OAuth App» → Homepage URL `http://localhost:8080`, Authorization callback URL `http://localhost:8080/auth/github/callback`. Скопируйте Client ID и сгенерируйте Client Secret. Логин пользователя — GitHub `login`.

**Google**: [console.cloud.google.com/apis/credentials](https://console.cloud.google.com/apis/credentials) → «Create Credentials» → «OAuth client ID» → тип «Web application» → в «Authorized redirect URIs» добавьте `http://localhost:8080/auth/google/callback`. Для продакшена сначала настройте экран согласия (OAuth consent screen). Логином пользователя становится часть e-mail до `@`.

**Yandex**: [oauth.yandex.ru](https://oauth.yandex.ru/) → «Создать приложение» → тип **«Для авторизации пользователей»** → платформа «Веб-сервисы» → Redirect URI `http://localhost:8080/auth/yandex/callback`. В доступе к данным отметьте **«Логин, имя и фамилия, пол»** и **«Адрес электронной почты»** (сервис запрашивает скоупы `login:info` и `login:email`). Скопируйте **ID** и **пароль приложения** (ClientID/Client secret) со страницы приложения.

Полученные значения задайте переменными окружения (или в `.env` — compose читает его автоматически):

```sh
export OAUTH_GITHUB_CLIENT_ID=...
export OAUTH_GITHUB_CLIENT_SECRET=...
# при запуске в Docker: PUBLIC_BASE_URL должен совпадать с реальным адресом,
# который видит браузер
export PUBLIC_BASE_URL=http://localhost:8080
make up
```

После этого на `/login` появятся кнопки входа. Изменить логин после первого входа нельзя.

### Production

- `PUBLIC_BASE_URL` должен указывать на реальный HTTPS-адрес, иначе redirect URI не совпадёт с ожидаемым провайдером.
- Обязательно `COOKIE_SECURE=true` (cookie сессий и OAuth-state отправляются только по HTTPS).
- Секреты провайдеров храните вне репозитория: через переменные окружения или secret manager, не в `.env` под git.

## Конфигурация

Переменные окружения: `DATABASE_URL` (обязательна), `HTTP_ADDR`, `ARCHIVES_DIR`, `MAX_MESSAGE_BYTES`, `MAX_API_LIMIT`, `DEFAULT_API_LIMIT`, `SHUTDOWN_TIMEOUT`, `RETENTION_INTERVAL`, `PUBLIC_BASE_URL`, `COOKIE_SECURE`, `SESSION_TTL`, а также пары OAuth-провайдеров (см. [Регистрация и OAuth](#регистрация-и-oauth)). Значения по умолчанию — в [.env.example](.env.example). Максимумы проверяются на старте. Крон retention запускается сразу при старте и затем с интервалом `RETENTION_INTERVAL`.

В production включите `COOKIE_SECURE=true` и используйте HTTPS.
