# ADR-009 — Realtime over SSE; in-process bus; TS reference frozen

- **Status**: accepted
- **Date**: 2026-07-15

## Context

The append hook already fires on every commit; realtime is pushing those events
to subscribers as they land — the headline Supabase-parity feature. A data
layer wants live updates so a client sees state changes without re-polling.

## Decision

`GET /v1/subscribe` — **Server-Sent Events**, not WebSocket. Reads are one-way
(the server pushes new events; clients still write via POST), and SSE is HTTP,
proxy-friendly, and reconnects natively with `Last-Event-ID`. WebSocket's
bidirectionality buys nothing here.

**Delivery = catch-up + live tail, ordered and gap-free.** On connect the
server replays the log from the cursor (`Last-Event-ID` header, else
`?fromGlobalSeq`), then tails live appends. Each frame's SSE id is its
`globalSeq` — the reconnect cursor. Correctness rests on globalSeq (the total
order), never on the bus: a slow subscriber is **dropped, not blocked**, and
reconnects to catch up from the log. No event is ever lost, only re-read. This
is exactly why globalSeq is an integer cursor (ADR-008).

**In-process bus, no broker.** Instance-per-app means a single process and a
single writer, so an in-memory per-tenant fan-out (`internal/bus`) suffices —
no Redis/Kafka. The append handler publishes after the fold; subscribers drain.
This is a direct dividend of the deployment model.

**Auth: service token only.** The raw log bypasses row policies, so a
subscription is a trusted-caller operation (same gate as append). The **SSE
consumer is the owning app (a server), not the browser** — so the "EventSource
can't set headers" problem does not apply: the app subscribes with a service
token in the `Authorization` header, applies its own policy, and relays to its
users over its own channel. foldbase never faces the browser directly. Tokens
in query strings are refused (they leak in logs). `?token=` is not supported.

**TS reference is frozen; realtime is Go-first.** The Go binary is the
production implementation; the TS reference is on the retirement path (ADR-001)
and is frozen at the core 58-check contract. Realtime is implemented in Go and
verified by a dedicated suite (`conformance/realtime.mjs`, 13 checks) plus TS &
Python client subscribe smokes. The core conformance suite stays a true
cross-implementation contract; the TS reference is not extended further.

## Consequences

- Clients gain `subscribe`: TS (`fb.subscribe(opts, onEvent)`, fetch-based,
  auto-reconnect from the last globalSeq, works in Node and the browser) and
  Python (`fb.subscribe(...)`, a blocking generator).
- Row-level realtime (policy-filtered read-model change subscriptions for
  direct end-user consumers) is **phase 2** — it needs per-subscriber policy
  evaluation and, for browsers, a fetch-stream client or a short-lived
  single-use subscription ticket. Not in this decision.
- Heartbeats (`: ping` every 25s) keep the stream alive through proxies;
  `X-Accel-Buffering: no` disables nginx buffering.

## Amendment — 2026-11-13: the bus wakes, the log delivers (issue #13)

The first implementation sent the bus's events themselves and dropped any at
or below the last globalSeq sent. Appends publish after their own commit and
fold, with nothing ordering them, so under concurrent appends seq 6 could
reach the bus before seq 5; 5 was then dropped as "overlap", and since the
client's cursor was already 6, a reconnect skipped it for good. On Postgres a
second path lost events too: an identity is assigned at insert, not at commit,
so a log read could see 10 committed while 9 was still in flight.

Two changes restore "ordered and gap-free" under concurrency:

- **The bus is a wake-up signal only.** A subscriber never sends what the bus
  hands it. On a wake-up (coalesced) and on every heartbeat it reads the log
  from its cursor, in globalSeq order, and sends that. A lost or reordered
  wake-up costs latency, never an event.
- **A tenant's appends commit in globalSeq order.** On Postgres the append
  transaction first takes `pg_advisory_xact_lock(hashtext('foldbase.append'),
  hashtext(tenant))`. The lock is transaction-scoped: commit, rollback or a
  dropped connection releases it, so it cannot be left held. Different
  tenants still append in parallel. SQLite and sqld already run one write
  transaction at a time and need nothing.

Proof: `TestSubscribeDeliversEveryEventUnderConcurrentAppends` (16 writers,
one reconnecting subscriber) requires every globalSeq in the log exactly once,
on SQLite and, with `FOLDBASE_TEST_PG`, on Postgres (`just test-go-pg`).
Before the change it lost about half the events on both; on Postgres the
cursor read without the lock still lost a few.

The bus stays in-process: one instance per app is still the model. Several
instances would need a cross-instance wake-up (Postgres `LISTEN/NOTIFY`);
until then an append on another instance reaches a subscriber at the next
heartbeat. The fold has the same ordering gap (#14) and the rebuild is not
atomic (#15); both are separate steps.
