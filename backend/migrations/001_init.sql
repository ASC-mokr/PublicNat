-- Миграция 001: структура БД платформы NAT-траверсала (RatHole Control Plane)
-- Требуется PostgreSQL 13+ (генерация UUID через gen_random_uuid())

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ============================================================
-- Таблица users (Пользователи)
-- ============================================================
CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         VARCHAR(255) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,          -- bcrypt
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users (email);

-- ============================================================
-- Таблица tunnels (Туннели / Сервисы)
-- ============================================================
CREATE TABLE IF NOT EXISTS tunnels (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         VARCHAR(64) NOT NULL,             -- латиница, цифры, дефис (проверяется на уровне приложения)
    type         VARCHAR(4) NOT NULL CHECK (type IN ('tcp', 'udp')),
    server_port  INTEGER NOT NULL UNIQUE CHECK (server_port BETWEEN 1 AND 65535),
    tunnel_token VARCHAR(128) NOT NULL UNIQUE,     -- криптографически стойкий токен (>= 32 символов)
    subdomain    VARCHAR(255) UNIQUE,              -- опциональный поддомен для HTTP/HTTPS
    created_at   TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Уникальный составной индекс: имя туннеля уникально в рамках пользователя
CREATE UNIQUE INDEX IF NOT EXISTS idx_tunnels_user_name ON tunnels (user_id, name);

-- Индекс на server_port для быстрой проверки занятости портов
-- (уникальность уже покрывается UNIQUE-ограничением, но явный индекс
-- ускоряет range-запросы при выборе свободного порта из диапазона)
CREATE INDEX IF NOT EXISTS idx_tunnels_server_port ON tunnels (server_port);

-- Индекс по user_id для выборки списка туннелей пользователя
CREATE INDEX IF NOT EXISTS idx_tunnels_user_id ON tunnels (user_id);
