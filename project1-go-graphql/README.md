# TaskFlow — Go + GraphQL + Postgres

A multi-user task manager. Backend is Go with a GraphQL API over Postgres; the frontend is a single HTML file. JWT auth, live updates via Server-Sent Events.

This is deliberately scoped as a portfolio project: it is **small enough to read end-to-end in an afternoon**, but exercises the things a reviewer actually cares about — schema design, authorization boundaries, database pooling, graceful shutdown, and a realistic tradeoff story.

---

## Run it

```bash
docker compose up --build
# open http://localhost:8080
```

Create an account, make a project, add tasks. Open the same project in a second browser tab and watch live updates flow across via SSE.

Without Docker:

```bash
export DATABASE_URL=postgres://taskflow:taskflow@localhost:5432/taskflow?sslmode=disable
export JWT_SECRET=dev-only-change-me
psql "$DATABASE_URL" -f migrations/001_init.sql
go run ./cmd/server
```

---

## Architecture

```
 ┌──────────────┐   HTTP/JSON   ┌─────────────────────────────────────┐
 │  web/ (HTML) │ ────────────▶ │            chi router               │
 │              │ ◀──────────── │  ├── auth.Middleware (JWT → ctx)    │
 │              │   SSE events  │  ├── POST /graphql  → graphql.Do    │
 └──────────────┘               │  ├── GET  /api/events → TaskPubSub  │
                                │  └── GET  /*  → static file server  │
                                └────────────┬────────────────────────┘
                                             │
                       ┌─────────────────────▼─────────────────────┐
                       │        internal/gql (schema + resolvers)  │
                       │  - builds GraphQL schema in Go            │
                       │  - routes to store / pubsub               │
                       └─────────────────────┬─────────────────────┘
                                             │
                       ┌─────────────────────▼─────────────────────┐
                       │            internal/store                 │
                       │  - ALL SQL lives here                     │
                       │  - returns *model.X or ErrNotFound        │
                       └─────────────────────┬─────────────────────┘
                                             │  pgx pool
                       ┌─────────────────────▼─────────────────────┐
                       │                 Postgres                  │
                       │   users, projects, tasks (+ indexes)      │
                       └───────────────────────────────────────────┘
```

### Layer responsibilities

| Layer | Knows about | Does NOT know about |
|---|---|---|
| `cmd/server` | Every other package (wiring only) | any business rule |
| `internal/gql` | `auth`, `store`, `model`, `pubsub` | SQL, HTTP |
| `internal/store` | SQL, `model`, `db` | GraphQL, HTTP, auth |
| `internal/auth` | JWT, bcrypt, context | DB rows |
| `internal/db` | pgx, env config | anything domain-specific |

The arrows only go one way. `store` has no idea a GraphQL layer exists — you could swap it for a REST API in an afternoon.

---

## Key design decisions

### 1. `graphql-go` instead of `gqlgen`

**Chose `graphql-go`:** the schema is built in plain Go code (see `internal/gql/schema.go`), so every type is something you can click through in an IDE. No code-generation step. For a project that's meant to be read and understood, this wins.

**What I gave up:** strong typing on resolver arguments. Incoming args are `map[string]interface{}` and we type-assert. `gqlgen` would generate strongly-typed arg structs.

**When I'd switch:** once the schema passes ~20 types, or the moment I need subscriptions over WebSockets. `gqlgen` has first-class support for both; `graphql-go` needs a second library for subscription transport.

### 2. Server-Sent Events for live updates, not WebSockets

The frontend gets real-time task updates via a plain HTTP streaming endpoint (`/api/events`). It is wired to an in-memory `TaskPubSub` that mutations publish to.

**Why SSE over WebSockets:**
- Plain HTTP — works through every corporate proxy, no Upgrade negotiation.
- Browser reconnects automatically on network blip.
- One-way server→client is all we need; WebSockets add complexity for a capability we don't use.

**Why this is NOT production-ready:**
- `TaskPubSub` is in-memory. If you run 3 replicas behind a load balancer, a mutation on pod A does **not** reach a subscriber on pod B.
- The fix at scale: swap the pubsub implementation for Redis Pub/Sub, NATS, or Postgres `LISTEN/NOTIFY`. The `TaskPubSub` struct is tiny and replaceable — that's by design.

### 3. Auth: JWT, no session table

Stateless JWTs. Pros: horizontal scaling is free. Cons: no revocation until expiry (7 days).

**Accepted tradeoff** for a task manager. For a system with sensitive operations (banking, medical) you'd want short-lived access tokens + refresh tokens + a revocation list.

**Password hashing:** bcrypt cost 12. Argon2id would be stronger; bcrypt won on "widely reviewed, in the standard crypto module, good enough for 2026."

**User enumeration:** login returns the same error message for "email not found" and "wrong password." Without this, anyone can probe whether a given email is registered.

### 4. Authorization: belt-and-braces, not one or the other

Two patterns, used together:

1. **Filter in SQL** where we can. `DeleteTaskByOwner` does `DELETE WHERE id = $1 AND project_id IN (SELECT id FROM projects WHERE owner_id = $2)`. This is atomic — no read-then-write race.
2. **Check in the resolver** where we can't. `resolveProject` fetches the row and then compares `OwnerID == uid` in Go. Slightly more code, but the error message is identical whether the project doesn't exist or belongs to someone else ("project not found"), avoiding information leakage.

### 5. UUIDs, not bigserial

`gen_random_uuid()` as the default. Reasons:
- **No ID-guessing attacks.** A leaked task ID does not help you enumerate other tasks.
- **Generatable client-side.** Useful for offline-first or optimistic UI.
- **No "hot tail" on the index.** Bigserial inserts all cluster on the right-hand side of the B-tree — under write load this becomes a contention point. UUIDs are spread out.

Tradeoff: UUIDs are 16 bytes vs 8, and random UUIDs are less cache-friendly than sequential ones. For this scale it doesn't matter. At TB-scale you'd reach for UUIDv7 (time-ordered UUIDs) to recover cache locality.

### 6. SQL migrations as plain `.sql`, loaded by Postgres on first boot

No migration library (goose, golang-migrate). A single file dropped into `/docker-entrypoint-initdb.d`. The moment we have two migrations, this breaks — and that's when a real migration tool earns its complexity. Not before.

### 7. `TaskStatus` as `TEXT CHECK`, not a Postgres `ENUM`

Postgres enums are tempting but painful to evolve — adding a value is fine, but removing or renaming one requires dropping and recreating the type. A `TEXT` column with a `CHECK (status IN (...))` constraint is easier to alter (just swap the constraint) and still gives the same input validation.

### 8. No dataloader — on purpose, for now

The current code has a classic N+1 risk: ask for 100 projects' owners and you get 100 user lookups. That's **intentionally** left in because:
- For the demo scale (a user with, say, 20 projects), it's not measurable.
- Adding a dataloader right now would add a dependency + mental overhead for a problem nobody has hit yet.

**When I'd fix it:** the moment the schema gets a multi-tenant query (e.g., "all tasks across all my projects"). I'd reach for `github.com/graph-gophers/dataloader` and batch owner fetches behind a per-request loader.

---

## What's deliberately NOT in this project

These are things a reviewer would notice are missing. I'd rather call them out than have you wonder.

- **Email verification & password reset** — would need an SMTP service + token table. Scope creep for a demo.
- **Rate limiting** — one `chi/middleware` line away. Not included to keep the wiring readable.
- **Tests** — would add `store_test.go` with testcontainers for Postgres, and resolver tests against a fake `Store` interface. I sketched the interface split mentally; it's the first thing I'd add.
- **Observability** — no Prometheus metrics, no structured logging, no tracing. The chi logger middleware gives basic request logs. Real system: swap in `slog` + OpenTelemetry.
- **Multi-user collaboration** — the data model is single-owner-per-project. A real task manager would have a `project_members` table with roles (owner/editor/viewer). Easy extension; not done here.

---

## File-by-file

```
cmd/server/main.go              HTTP wiring, graceful shutdown, SSE handler
graph/schema.graphql            Canonical schema (human-readable reference)
internal/auth/auth.go           JWT issue/parse + bcrypt
internal/auth/middleware.go     Reads Authorization header into context
internal/db/db.go               pgx pool config
internal/gql/schema.go          GraphQL schema built in Go (graphql-go)
internal/gql/resolvers.go       Query + mutation resolvers
internal/gql/pubsub.go          In-memory fanout for SSE
internal/model/model.go         Domain structs
internal/store/store.go         All SQL
migrations/001_init.sql         DB schema
web/index.html                  Frontend (single file, no build step)
docker-compose.yml              Postgres + app
Dockerfile                      Multi-stage build
```

`graph/schema.graphql` is **documentation**. The actual schema used at runtime is the Go code in `internal/gql/schema.go` (graphql-go builds it there). Keeping the `.graphql` file in sync by hand is fine at this size; for a larger schema I'd generate one from the other.

---

## Scaling notes (what I'd change at 1000× load)

1. **Pub/sub** → Redis or NATS (replaces `TaskPubSub`, no other code changes).
2. **DB pool** → raise `MaxConns`, add PgBouncer between app and Postgres so we don't waste DB connections on idle app replicas.
3. **N+1** → dataloader per request.
4. **Cache read-heavy queries** (projects list) in Redis, invalidated on mutation.
5. **Move SSE behind a connection-aware LB** (or switch to WebSockets via a dedicated gateway like Ably/Pusher/Centrifugo if we need cross-pod fanout without running our own Redis).
6. **Split read and write DBs** once the write path becomes the bottleneck.
7. **Add a background worker** for anything that doesn't need to block a mutation (email, indexing).
