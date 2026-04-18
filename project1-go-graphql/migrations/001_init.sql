-- Migration 001: initial schema.
-- Run against Postgres with `psql -f migrations/001_init.sql` or via docker-compose.
-- All IDs are UUIDs — see the README for why we don't use bigserial.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";  -- provides gen_random_uuid()

CREATE TABLE users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,          -- UNIQUE + indexed automatically
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,                 -- bcrypt hash, never the raw password
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE projects (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    description TEXT,                            -- nullable; matches the GraphQL schema
    owner_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Index the foreign key. Postgres does not auto-index FKs, and we filter by
-- owner_id in almost every project query.
CREATE INDEX idx_projects_owner ON projects(owner_id);

-- TaskStatus as a CHECK constraint instead of a Postgres ENUM.
-- CHECK is easier to evolve: altering an ENUM type requires a transaction-
-- blocking DDL, whereas altering a CHECK is just a constraint swap.
CREATE TABLE tasks (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id  UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    description TEXT,
    status      TEXT NOT NULL DEFAULT 'TODO'
                CHECK (status IN ('TODO', 'IN_PROGRESS', 'DONE')),
    assignee_id UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_assignee ON tasks(assignee_id);
