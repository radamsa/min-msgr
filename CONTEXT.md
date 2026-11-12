# CONTEXT — текущее состояние проекта

Дата фиксации: 2026-11-12.

Здесь только то, что есть сейчас.
Предельный размер файла: 16Kb.

## Реализовано

- Go-модуль `github.com/dav/min-msgr`, Go 1.27.
- PostgreSQL-схема и встроенные миграции (`internal/db/migrations/0001_init.sql`):
  users, oauth_accounts, sessions, chats, api_keys, messages, admin_settings,
  user_overrides, archives.
- `messages.seq` — глобальный `BIGSERIAL`; уникальность в чате через
  `UNIQUE (chat_id, seq)`. Идемпотентность агентских сообщений — частичный
  уникальный индекс `(chat_id, client_message_id)`.
- Конфиг из env с валидацией на старте (`internal/config`).
- REST API для агентов (`internal/api`):
  - `POST /api/v1/messages`;
  - `GET /api/v1/messages` с `limit`, `cursor`, `since`;
  - Bearer-аутентификация по SHA-256 хэшу ключа;
  - opaque-курсоры, пагинация назад/вперёд, `next_cursor`.
- PostgreSQL-хранилище (`internal/store/postgres`).
- Сервис создания пользователя, чата и ключа (`internal/service`).
- OAuth-вход GitHub/Google/Yandex (`internal/oauth`) и регистрация при первом
  входе (`internal/service/oauth.go`); CSRF-state в короткоживущей cookie.
- Серверные сессии (`internal/service/sessions.go`): случайный токен, в БД
  только SHA-256, cookie HttpOnly/SameSite=Lax, срок `SESSION_TTL`.
- Веб-слой (`internal/web`): `/login`, `/auth/{provider}`,
  `/auth/{provider}/callback`, `/logout`, список чатов, страница чата
  `/chats/{chatID}`, управление ключами (создание, ротация, отзыв, настройка
  лимитов чата), скачивание архивов и админ-панель `/admin`; SSR-шаблоны.
- WebSocket-чат (`internal/web/ws.go`, `/ws`): события `messages.subscribe`
  (последняя страница, опциональные `initial`/`since`), `messages.before`
  (старые страницы по opaque-курсору), `message.send`; сервер шлёт
  `messages.page`, `message.created`, `message.ack`, `message.error`,
  `history.trimmed`, `chat.deleted`. Авторизация — сессионной cookie, доступ
  только к своим чатам. Клиент `static/chat.js`: reconnect с backoff,
  дедупликация по `seq`, клиентская sanitization HTML, повторная отправка
  недоставленных сообщений.
- Realtime-hub (`internal/realtime`): fan-out по `chat_id`, неблокирующая
  публикация (медленный клиент пропускает события, буфер 64); один hub общий
  для REST API, WebSocket и retention.
- Управление ключами (`internal/service/keys.go`): список чатов с последним
  сообщением и эффективными лимитами, ротация ключа in-place, настройка
  пользователем `days_limit`/`bytes_limit` чата с зажимом до админских
  максимумов (нули/некорректные значения сохраняют текущие).
- Байтовые лимиты и trimming (`internal/service/limits.go`): после вставки
  сообщения `LimitsService.TrimAfterMessage` в транзакции с advisory-локом
  пользователя удаляет самые старые сообщения чата по `bytes_limit`, затем —
  глобально по времени среди чатов пользователя по
  `max_total_bytes_per_user`; возвращает `[]domain.TrimResult`.
- Событие `history.trimmed` (`chat_id`, `deleted`, `through_seq`) публикуется
  после любого trimming (REST, WebSocket, retention-крон).
- Админ-панель (`internal/web/admin.go`, `/admin`): глобальные лимиты
  (`admin_settings`), список пользователей с числом чатов, блокировка
  (с удалением живых сессий) и удаление пользователей (каскад), per-user
  переопределения лимитов (`user_overrides`). Доступ только для `is_admin`;
  содержимого чатов админ не видит.
- Консольная утилита `cmd/min-msgr-admin`: `promote`/`demote`, `block`/
  `unblock`, `delete-user`, `set-limits`, `set-override`/`clear-override`,
  `show`. Пользователь задаётся как `login#NNN`, числовой ID или логин
  (при неоднозначности — ошибка со списком ID).
- Настройки пользователя: на странице ключей видны эффективные лимиты
  (ключи, дни, байт/чат) и общий байтовый лимит с текущим потреблением —
  только просмотр, меняет админ.
- Поиск пользователей в `service.UserService`: `ByPublicID`, `ByID`,
  `ByLogin` (для утилиты и админки).
- Архивация при отзыве (`internal/service/archives.go`): чат блокируется
  `FOR UPDATE`, пишется zip с `.md`, архив хранится 30 дней, файл удаляется
  при ошибке БД; скачивание проверяет владельца и путь внутри `ARCHIVES_DIR`.
- Идемпотентный retention-крон (`internal/service/retention.go`): удаляет
  сообщения старше `chats.days_limit` (с публикацией `history.trimmed`),
  просроченные сессии и архивы после `expires_at` (файл удаляется до строки
  БД; путь ограничен `ARCHIVES_DIR`). Запускается сразу при старте сервера и
  далее с интервалом `RETENTION_INTERVAL`, логирует ошибки и результаты.
- Sanitization Markdown (`internal/markdown`): goldmark + bluemonday,
  запрещены raw HTML, ссылки, изображения и автолинки.
- Dev-утилита `cmd/min-msgr-devkey` для создания пользователя/чата/ключа.
- Unit- и интеграционные тесты; интеграционные используют изолированную
  схему через `internal/testdb`.
- `compose.yaml` с PostgreSQL на порту 55432, `Makefile`, `README.md`.

## Проверено

- `go test ./...` — unit-тесты.
- `make test-integration` — миграции, store, service (включая admin, limits,
  retention), OAuth-flow, веб-управление ключами/архивами/лимитами,
  WebSocket-чат и админ-панель (доступ, настройки, блокировка, override) на
  реальном PostgreSQL.
- Ручной smoke: `/healthz`, `/login`, OAuth callback с тестовым провайдером,
  `/logout`, создание/ротация/отзыв ключа, скачивание архива,
  `POST /api/v1/messages`, `GET /api/v1/messages`, идемпотентный повтор,
  forward-курсор, все команды `min-msgr-admin`.

## Ещё нет

- CI.
- Rate limits / CORS (по необходимости — план помечает их опциональными).

## Ближайший порядок работ

1. CI (lint + unit + интеграционные).
2. Опционально: HTMX-полировка интерфейса, rate limits.
