# AGENTS.md — foldbase

Operating manual for AI coding agents. Read this before wiring an app to
foldbase or changing it. Companion: [README.md](./README.md) (usage, curl
quickstart) · [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) (layout) ·
[docs/adr/](./docs/adr/README.md) (decisions 001–008).

## What this is

The append-only event log + its folded read models + one generic, policy-gated
query endpoint. **Contract-first**: [openapi.yaml](./openapi.yaml) is the
source of truth; the [conformance suite](./conformance/run.mjs) (58 HTTP
checks) locks behavior. Two implementations green it identically:

- **Go** (`go/`) — the production binary. `database/sql`, no ORM (ADR-006).
- **TS reference** (`src/`) — Hono; retires once the Go image ships (ADR-001).

Deployment model: **no shared central instance** — each app deploys its own
foldbase as a private sibling. The word "eventstore" is retired; the project,
image, packages, and env vars are all `foldbase`.

## The three rules (never break these)

1. **Log first, always.** The append commits before the fold runs. Never
   reorder; never make a fold failure fail an append; never feed a projection
   back into event production. Stale views are repairable (`/admin/rebuild`);
   fabricated or lost events are not.
2. **No domain logic in the service.** Folds are declarative rules
   (`upsert`/`inc`/`delete`). If a fold needs cross-aggregate logic, it belongs
   in the owning app (mode B, future), not in the rules engine.
3. **Table taxonomy is load-bearing.** `events` = truth; `_projections` /
   `_policies` / `_rpc` = definitions (control-plane data, never queryable);
   `read_<name>` = disposable. The query engine resolves names only through
   the registry; identifiers are regex-validated (`^[a-z][a-z0-9_]*$`) — no
   code path may ever place client text in an SQL identifier or fragment.

## Contracts you must not break

- Everything in `openapi.yaml`. A change to the HTTP surface without a spec +
  conformance update is an incomplete change.
- **Auth** (ADR-002/003/004/005): `FOLDBASE_AUTH=none|service-jwt|user-jwt`,
  fail-closed boot; control plane needs a `type:"service"` token; service
  tokens select tenant via `X-Tenant-ID` (required, no default; `org_id`
  pins); user tokens are query-only; **no policy bypass for anyone**.
- **Envelope** (ADR-008): `stream_type` is fixed by the stream's first append;
  event ids are UUIDv7 (client MAY supply, server validates); `globalSeq` is
  the total order; log reads page (default 1000, max 10000).
- Tenant isolation is structural (`tenant = ?` AND-ed everywhere) — never via
  policy text.

## How to wire an app (the pattern to replicate)

See [examples/taskboard-web](./examples/taskboard-web/server.mjs) — the
realistic owning-app: it holds the service token, registers definitions on ITS
boot (idempotent), emits via the typed catalog, and forwards the end-user
identity (`X-Auth-*`) so owner policies scope rows. TS authoring
(`defineAggregate` + `defineProjection`) infers read-model columns from event
schemas — prefer it over hand-written wire defs.

## Build / verify / ship

```bash
just build-go && just test-go     # binary + Go unit tests
just conformance                  # BOTH impls must green all checks
just test-all                     # + TS & Python client smokes
just dev-web                      # taskboard UI on :4000 (own sibling, file db)
just release-docker <tag>         # ghcr.io/dotlabshq/foldbase:<tag>
```

Any behavior change ⇒ update `openapi.yaml` + add a conformance check + green
both implementations. Decisions of record go to `docs/adr/`.
