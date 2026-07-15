# foldbase — architecture (contract-first, multi-language)

The service is the append-only event log + its materialized read models + a
generic query surface. It is becoming the **universal data-access layer** for
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
| `docs/adr/` | Decisions 001–007 (contract-first, auth modes, planes, tenant scoping, no-bypass, Go/no-ORM, client SDKs). |
| `conformance/` | `run.mjs` — boots a server binary and asserts the contract over HTTP. The behavior lock. |
| `src/` | TS **reference** implementation (Hono). Stays until Go parity (ADR-001). |
| `go/` | Go implementation — single static binary, `database/sql`, no ORM (ADR-006). |
| `clients/ts` | TS client SDK — HTTP + zod authoring layer. Zod only, no drizzle/libsql. |
| `clients/python` | Python client SDK — stdlib only, dependency-free. |
| `examples/taskboard-ts`, `examples/taskboard-py` | Runnable kanban demos exercising the whole surface via each client. |

## The env contract (every implementation honors it)

| Var | Meaning |
|---|---|
| `PORT` | listen port (default 3001) |
| `DB_URL` | libsql url; `:memory:` or `file:…` (Go build); TS also does Flect/sqld resolution |
| `FOLDBASE_AUTH` | `none` \| `service-jwt` \| `user-jwt` (ADR-002) |
| `FOLDBASE_JWT_SECRET` | HS256 realm secret (required in secured modes) |
| `FOLDBASE_ADMIN_TOKEN` | optional control-plane gate in `none` mode (ADR-003) |

## Run it

```bash
just conformance     # both impls must green the same suite
just test-all        # conformance + TS & Python client smokes
just demo-ts         # taskboard over the Go binary, via the TS client
just demo-py         # taskboard over the Go binary, via the Python client
```

Current status: **TS reference and Go both green 49/49 conformance checks;**
both client SDKs pass their smokes against both implementations.

## Data model recap

- `events` — the log, append-only, the truth. Multi-tenant, optimistic
  concurrency (`expectedVersion` → 409), global order (`globalSeq`).
- `_projections` / `_policies` / `_rpc` — definitions (data, control-plane
  managed, never queryable).
- `read_<name>` — disposable derived read models; rebuild anytime.

Log-first is the iron rule: the fold runs after the append commits; a fold
failure only leaves a projection stale (`projected: false`), never invents or
loses a fact.

## Roadmap (post-parity)

1. Republish `@baseworks/readmodel` (structured-fold coercion) and pin the TS
   service to the workspace version.
2. Retire the TS service; ship the Go image as `ghcr.io/dotlabshq/foldbase`.
3. Batch/streamed rebuild + atomic projection swap (scale).
4. Realtime (SSE/webhook off the append hook) — the Supabase-parity headline.
5. sqld → PostgreSQL dialect adapter (Go `database/sql`, invisible to clients).
6. HS256 → EdDSA + `kid` rotation.
