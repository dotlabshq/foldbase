# foldbase — architecture (contract-first, multi-language)

The service is the append-only event log + its materialized read models + a
generic query surface + realtime. It is the **universal data-access layer** for
every project (a Supabase/PostgREST role), so the design is contract-first and
polyglot.

```
                       openapi.yaml  ← the contract (source of truth, ADR-001)
                            │
        ┌───────────────────┼───────────────────┐
        │                   │                    │
   TS reference         Go impl              client SDKs
  (src/, Hono)     (go/, net/http)      (clients/ts, clients/python)
        │                   │                    │
        └──────── conformance/run.mjs ───────────┘
              (language-agnostic HTTP suite — all must green)
```

## Layout

| Path | What |
|---|---|
| `openapi.yaml` | The v1 contract. Every impl + client targets it. |
| `docs/adr/` | Decisions 001–010 (contract-first, auth modes, planes, tenant scoping, no-bypass, Go/no-ORM, client SDKs, stream_type, realtime, postgres). |
| `conformance/` | `run.mjs` (58 checks) + `realtime.mjs` (13 checks) — boot a server binary and assert the contract over HTTP. The behavior lock. |
| `src/` | TS **reference** implementation (Hono). Frozen at the core contract, on the retirement path (ADR-001/009). SQLite only. |
| `go/` | Production implementation — single ~14 MB static binary, `database/sql`, no ORM (ADR-006). Internals below. |
| `go/internal/{store,readmodel}` | Event-log repo + the mode-A rules/query engine. |
| `go/internal/dialect` | The thin SQLite/PostgreSQL adapter — placeholder rewrite, type map, DDL, introspection (ADR-010). |
| `go/internal/bus` | In-process pub/sub for realtime SSE fan-out (ADR-009). |
| `go/internal/{auth,jwt,httpapi}` | Trust boundary, HS256 verify, HTTP surface. |
| `clients/ts` | TS client SDK — HTTP + typed authoring (event schemas drive columns). Zod only. |
| `clients/python` | Python client SDK — stdlib-only core; optional pydantic authoring extra. |
| `examples/taskboard-{web,ts,py}` | Runnable kanban demos exercising the whole surface. |

## The env contract (every implementation honors it)

| Var | Meaning |
|---|---|
| `PORT` | listen port (default 3001) |
| `DB_URL` | `postgres://…` (Go, pgx) · `:memory:` / `file:…` (SQLite/libsql) · TS also resolves Flect/sqld |
| `FOLDBASE_AUTH` | `none` \| `service-jwt` \| `user-jwt` (ADR-002) |
| `FOLDBASE_JWT_SECRET` | HS256 realm secret (required in secured modes) |
| `FOLDBASE_ADMIN_TOKEN` | optional control-plane gate in `none` mode (ADR-003) |

## HTTP surface (openapi.yaml)

- **Streams (data):** `POST/GET /v1/streams/:id`, `/version`, `GET /v1/events`
  (global order, `?type=` category, cursor + limit), `/by-correlation/:id`
- **Realtime (data):** `GET /v1/subscribe` — SSE; catch-up from a cursor then
  live tail; `?type=` filter; service-token gated (ADR-009)
- **Query (data):** `POST /v1/query/:name` — deny-by-default, policy-scoped
- **Definitions (control):** `PUT /v1/projections`, `PUT /v1/policies`
- **Admin (control):** `POST /admin/reload`, `POST /admin/rebuild`

## Run it

```bash
just conformance     # both impls green the same 58 checks
just conformance-pg  # the Go impl against a real Postgres (needs a running instance)
just realtime        # SSE conformance (13 checks, Go)
just test-all        # conformance + realtime + TS & Python client + subscribe smokes
just dev-web         # taskboard demo UI → http://localhost:4000
```

Current status: **TS reference and Go both green all 58 conformance checks;**
the Go impl also greens them against **real PostgreSQL** and greens the 13
realtime checks; both client SDKs pass their smokes (query + subscribe) against
both implementations.

## Data model recap

- `events` — the log, append-only, the truth. Multi-tenant, optimistic
  concurrency (`expectedVersion` → 409), global order (`globalSeq`), UUIDv7
  ids, `stream_type` category (ADR-008).
- `_projections` / `_policies` / `_rpc` — definitions (data, control-plane
  managed, never queryable).
- `read_<name>` — disposable derived read models; rebuild anytime.

Log-first is the iron rule: the fold runs after the append commits; a fold
failure only leaves a projection stale (`projected: false`), never invents or
loses a fact.

## Storage dialects (ADR-010)

Clients speak HTTP, never SQL, so the store is a swappable back end. The engine
emits `?`-placeholder SQL and lowercase column types; `internal/dialect`
rewrites placeholders (`$n` for Postgres), maps types (`integer`→BIGINT on
Postgres so epoch-ms values don't overflow), owns the events DDL
(IDENTITY vs AUTOINCREMENT), introspection, and unique-violation detection. The
same conformance suite proves behavioral identity on SQLite and Postgres.

## Roadmap

1. **[user]** Publish `github.com/baseworks/foldbase`; release the Go image.
2. Retire the TS reference; the Go image is the sole implementation.
3. `payload`/`metadata` TEXT → `jsonb` (Postgres-only, indexed JSON queries).
4. Batch/streamed rebuild + atomic projection swap (scale).
5. HS256 → EdDSA + `kid` rotation.
6. Phase-2 realtime: row-level (policy-filtered read-model change) subscriptions
   for direct end-user consumers.

Done: contract-first + conformance (ADR-001) · auth modes / planes / tenant
scoping / no-bypass (002–005) · Go, no ORM (006) · TS + Python clients (007) ·
UUIDv7 + stream_type (008) · realtime SSE (009) · PostgreSQL dialect (010).
