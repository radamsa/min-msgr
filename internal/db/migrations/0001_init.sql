CREATE TABLE users (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    oauth_login TEXT NOT NULL,
    public_id TEXT NOT NULL UNIQUE,
    email TEXT,
    is_admin BOOLEAN NOT NULL DEFAULT FALSE,
    is_blocked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (length(oauth_login) BETWEEN 1 AND 255),
    CHECK (public_id ~ '^.+#[0-9]{3}$')
);

CREATE INDEX users_oauth_login_idx ON users (oauth_login);

CREATE TABLE oauth_accounts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    provider_user_id TEXT NOT NULL,
    email TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_user_id),
    CHECK (provider <> ''),
    CHECK (provider_user_id <> '')
);

CREATE INDEX oauth_accounts_user_id_idx ON oauth_accounts (user_id);

CREATE TABLE sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (octet_length(token_hash) = 32),
    CHECK (expires_at > created_at)
);

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE chats (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    days_limit INT NOT NULL,
    bytes_limit BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (label <> ''),
    CHECK (days_limit > 0),
    CHECK (bytes_limit > 0)
);

CREATE INDEX chats_user_id_idx ON chats (user_id);

CREATE TABLE api_keys (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chat_id BIGINT NOT NULL UNIQUE REFERENCES chats(id) ON DELETE CASCADE,
    label TEXT NOT NULL,
    key_hash BYTEA NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (label <> ''),
    CHECK (octet_length(key_hash) = 32)
);

CREATE TABLE messages (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    chat_id BIGINT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    seq BIGSERIAL NOT NULL,
    author_type TEXT NOT NULL,
    agent_name TEXT,
    body TEXT NOT NULL,
    bytes BIGINT GENERATED ALWAYS AS (octet_length(body)) STORED,
    client_message_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (chat_id, seq),
    CHECK (author_type IN ('user', 'agent')),
    CHECK (
        (author_type = 'agent' AND agent_name IS NOT NULL
            AND agent_name ~ '^[a-zA-Z0-9_-]{1,16}$')
        OR (author_type = 'user' AND agent_name IS NULL)
    ),
    CHECK (body <> ''),
    CHECK (client_message_id IS NULL
        OR octet_length(client_message_id) BETWEEN 1 AND 128)
);

CREATE INDEX messages_chat_created_at_idx ON messages (chat_id, created_at);
CREATE INDEX messages_created_at_idx ON messages (created_at);
CREATE UNIQUE INDEX messages_chat_client_message_id_idx
    ON messages (chat_id, client_message_id)
    WHERE client_message_id IS NOT NULL;

CREATE TABLE admin_settings (
    id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    max_keys_per_user INT NOT NULL DEFAULT 2 CHECK (max_keys_per_user > 0),
    max_days_limit INT NOT NULL DEFAULT 365 CHECK (max_days_limit > 0),
    max_bytes_per_chat BIGINT NOT NULL DEFAULT 10485760
        CHECK (max_bytes_per_chat > 0),
    max_total_bytes_per_user BIGINT NOT NULL DEFAULT 52428800
        CHECK (max_total_bytes_per_user > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO admin_settings (id) VALUES (TRUE);

CREATE TABLE user_overrides (
    user_id BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_keys_per_user INT CHECK (max_keys_per_user IS NULL OR max_keys_per_user > 0),
    max_days_limit INT CHECK (max_days_limit IS NULL OR max_days_limit > 0),
    max_bytes_per_chat BIGINT CHECK (max_bytes_per_chat IS NULL OR max_bytes_per_chat > 0),
    max_total_bytes_per_user BIGINT
        CHECK (max_total_bytes_per_user IS NULL OR max_total_bytes_per_user > 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE archives (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chat_id BIGINT NOT NULL UNIQUE,
    label TEXT NOT NULL,
    file_path TEXT NOT NULL UNIQUE,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL,
    CHECK (label <> ''),
    CHECK (file_path <> ''),
    CHECK (expires_at > created_at)
);

CREATE INDEX archives_user_id_idx ON archives (user_id);
CREATE INDEX archives_expires_at_idx ON archives (expires_at);
