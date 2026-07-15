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
