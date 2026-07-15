# foldbase — Design Record (Spek)

The **Design Record** section of the foldbase Spek (Open-Spek v0.1): the locked
engineering decisions with rationale, the architecture, trust boundaries, and
the deliberately rejected alternatives. This file is a **consolidating index**,
not a rewrite — the ratified decisions live as ADRs and the interface as the
OpenAPI contract; this record names them and states the shape.

- Interface contract (the machine-readable API): [`openapi.yaml`](../openapi.yaml)
- Full decisions with rationale + rejected alternatives: [`docs/adr/`](./adr/README.md)
- Vision: [`../MANIFESTO.md`](../MANIFESTO.md) · Acceptance: [`../loop/ACCEPTANCE.md`](../loop/ACCEPTANCE.md)

## Architecture

```
emit(event) ──▶ fold (rules as data) ──▶ read_<name> tables ──▶ POST /v1/query/:name
     │                          │                                        ▲
     │                          └──▶ in-process bus ──▶ GET /v1/subscribe (SSE)
     └── events table (append-only truth) ──── POST /admin/rebuild ──────┘
```

- **events** — the log: multi-tenant, append-only, optimistic concurrency
  (`expectedVersion` → 409), global total order (`globalSeq`), UUIDv7 ids,
  `stream_type` category.
- **_projections / _policies / _rpc** — definitions as data (control-plane
  managed, never queryable).
- **read_&lt;name&gt;** — disposable derived read models; rebuilt by replaying the log.

Two implementations behind one contract: **`go/`** (production, single static
binary, `database/sql`, no ORM) and **`src/`** (TS reference, frozen, on the
retirement path). Clients: **`clients/ts`** (typed authoring), **`clients/python`**
(stdlib core + optional pydantic authoring). Storage: SQLite/libsql or
PostgreSQL — invisible to clients.

## Locked decisions

Each is ratified as an ADR carrying full rationale **and** the rejected
alternatives (Open-Spek requires rejected alternatives to be explicit — they are,
per ADR).

| # | Decision | ADR |
|---|---|---|
| 1 | **Contract-first + language-agnostic conformance** is the source of truth; implementations are generated/verified against it. | [001](./adr/001-contract-first.md) |
| 2 | **Declared auth modes** (`FOLDBASE_AUTH none\|service-jwt\|user-jwt`), fail-closed in production. | [002](./adr/002-auth-modes.md) |
| 3 | **Data/control plane split** via a `type:"service"` claim. | [003](./adr/003-plane-separation.md) |
| 4 | **Service token = app identity**; tenant chosen per request via `X-Tenant-ID` (required, no default; `org_id` pins). | [004](./adr/004-service-token-tenant-scope.md) |
| 5 | **No policy bypass** for anyone; user tokens query-only in v1. | [005](./adr/005-no-policy-bypass.md) |
| 6 | **Go, no ORM**; SQL-injection defense as explicit, tested invariants. | [006](./adr/006-go-no-orm.md) |
| 7 | **One client package per language**; engines live in the service. | [007](./adr/007-client-sdks.md) |
| 8 | **`stream_type`** — denormalized, client-supplied, NOT NULL; log reads page. | [008](./adr/008-stream-type.md) |
| 9 | **Realtime over SSE**; in-process bus; TS reference frozen. | [009](./adr/009-realtime-sse.md) |
| 10 | **PostgreSQL via a thin dialect adapter**; clients unchanged. | [010](./adr/010-postgres-dialect.md) |

## Trust boundaries

- **The instance boundary.** No shared central store: each app's foldbase holds
  only that app's data. A leaked credential's blast radius is one app. Several
  decisions (service-token tenant selection, no per-tenant token minting) are
  sound *only because of* this boundary (ADR-004).
- **The auth boundary.** Tenant and capabilities derive from a verified token
  (secured modes) or `X-Tenant-ID` (dev/none). `X-Auth-*` end-user identity is
  honored only from a **service token** (a trusted caller that verified the
  end user itself); user tokens derive identity from their own claims and are
  query-only. Control-plane routes require a service token. (ADR-002/003/004/005)
- **The SQL boundary.** Identifiers are validated (`^[a-z][a-z0-9_]*$`) and
  resolved only through the registry; values are always bound parameters;
  policy `using` fragments are operator-authored config on the control plane.
  Client text never reaches an SQL identifier or fragment. (ADR-006)
- **The row boundary.** Deny-by-default select policies; `tenant = ?` AND-ed
  structurally into every query and fold. No bypass. (ADR-005)

## Rejected alternatives (index)

Recorded in full inside the ADRs; summarized here for the Spek conformance
checklist:

- **A shared central eventstore** for all apps — rejected for blast-radius and
  ownership reasons (MANIFESTO; ADR-004 depends on the sibling model).
- **An ORM** — rejected: the engine's security model is "we construct all SQL";
  an ORM would obscure, not strengthen it (ADR-006).
- **WebSocket for realtime** — rejected: reads are one-way; SSE is HTTP,
  proxy-friendly, reconnects natively (ADR-009).
- **A separate admin secret** for the control plane — rejected in favor of a
  claim on the same realm token (ADR-003).
- **Per-tenant service tokens** — rejected: friction without a security gain
  inside the trust boundary (ADR-004).
- **Implicit service-role policy bypass** (Supabase's foot-gun) — rejected: an
  explicit `role:"service"` policy keeps the deny-by-default rule exception-free
  (ADR-005).
- **A `stream_type` prefix on stream ids / a normalized streams table** —
  rejected: the read-model table name carries the type; a denormalized column
  is cheaper and JOIN-free (ADR-008).
- **jsonb for payload/metadata now** — deferred: TEXT keeps SQLite/Postgres
  parity; jsonb is a future Postgres-only optimization (ADR-010).

## Regeneration test

*Could a competent team (or an agent loop) reproduce foldbase from this Spek
alone?* Yes: the Vision fixes intent and refusals, this Design Record + the ADRs
fix every decision with rationale, `openapi.yaml` fixes the interface, and
`loop/ACCEPTANCE.md` names a machine gate that proves conformance. The existing
`go/` and `src/` implementations are two such regenerations behind one contract.
