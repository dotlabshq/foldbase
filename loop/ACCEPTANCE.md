# foldbase — Acceptance Criteria (Spek)

The **Acceptance Criteria** section of the foldbase Spek (Open-Spek v0.1): the
testable, machine-checkable criteria and the **verification oracle (gate)** that
proves them. Per the standard, every criterion is mechanically verifiable and
the gate command is named.

## The gate

```
GATE_CMD = just gate
```

`just gate` runs, in order, and exits non-zero on the first failure:

| Step | Command | Proves |
|---|---|---|
| unit | `just test-go` | Go engine internals (store, auth, query, dialect) |
| contract | `just conformance` | **both** implementations (Go + TS reference) green the 58 HTTP contract checks over real HTTP |
| realtime | `just realtime` | the 13 SSE checks against the Go binary |

Exit 0 ⇔ foldbase upholds [`openapi.yaml`](../openapi.yaml). The contract step
is the heart: a language-agnostic runner boots a server **binary** and asserts
behavior over HTTP, so the gate is implementation- and language-independent —
the same oracle validates any future implementation (`foldbase-<lang>`) or any
storage dialect.

Extended oracle (client SDKs + Postgres), run in CI:

```
just test-all          # gate + TS & Python client query/subscribe smokes
just conformance-pg    # the 58 checks against a real PostgreSQL (needs an instance)
```

## Criteria (all machine-checked by the gate)

The conformance suite ([`conformance/run.mjs`](../conformance/run.mjs),
[`realtime.mjs`](../conformance/realtime.mjs)) is the authoritative,
executable form. Grouped:

**Log & concurrency**
- Append assigns monotonic `version` per stream and global `globalSeq`; events carry UUIDv7 ids.
- Client-supplied event id is honored verbatim; a malformed id is rejected (400).
- `expectedVersion` mismatch → 409 with the actual version (never a 500).
- Stream history, global-order read, and by-correlation read return events in order.

**Read models & query**
- `PUT /v1/projections` is idempotent and auto-rebuilds the calling tenant ("events first, definition later" is safe).
- Folds apply `upsert` / `inc` / `delete`; structured payload values are stored as JSON text.
- `POST /v1/query/:name`: `select` / nested `where` / `sort` / `limit` over whitelisted columns only.
- `POST /admin/rebuild` replays the log deterministically into the read models.

**Security invariants**
- Deny-by-default: no policy for the caller ⇒ 403 (not "all rows").
- `tenant = ?` is AND-ed structurally; cross-tenant reads see nothing.
- Hostile input (SQL in `select`, unknown/forbidden column, `_`-prefixed table) ⇒ 400/404 and the store is undamaged.
- Auth modes behave per ADR-002/003/004/005: fail-closed boot; service-token tenant selection + `org_id` pinning; user tokens query-only; control plane requires a service token.

**stream_type & paging**
- `stream_type` is fixed by the stream's first append; a conflicting later type ⇒ 400.
- `GET /v1/events?type=…` is a read-model-free category read; log reads page (default limit 1000).

**Realtime (SSE)**
- Subscribe is service-token gated (401/403 otherwise).
- Live delivery in order; catch-up from a cursor; reconnect via `Last-Event-ID` resumes gap-free; `?type=` filter; tenant isolation.

## Status

At the time of writing the gate is **green**: Go unit (4 packages) · conformance
**58/58** on both the Go binary and the TS reference · realtime **13/13** ·
and **58/58** against a real PostgreSQL 16 (extended oracle). Two implementations
and two storage dialects satisfy one contract — the acceptance proof of a Spek.
