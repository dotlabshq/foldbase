# ADR-008 — `stream_type`: denormalized, client-supplied, NOT NULL

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Stream ids are bare UUIDv7 (no type prefix — the read model's table name
already carries the type). That leaves the raw log without a self-describing
notion of what kind of aggregate a stream is, and leaves category reads
("every task stream") reachable only through a read model. A universal data
layer will want category reads, category subscriptions (realtime), and
type-scoped authorization — and envelope columns are far cheaper to add
before data exists than after (append-only logs can't rewrite history).

## Decision

`events.stream_type TEXT NOT NULL DEFAULT ''` — with three deliberate
properties:

1. **Denormalized** — repeated on every event of the stream rather than kept
   in a separate `streams` table. Appends stay single-insert (no second table
   in the transaction), category reads are one WHERE with no JOIN, and
   replication/backup stay trivial. The redundancy is bytes; the invariant
   (one type per stream) is enforced at append time, not by normalization.
2. **Client-supplied** — the aggregate's kind is knowledge the app has at
   creation; the server can't infer it (event-type-name conventions are
   guessable but fragile). Wire: optional `streamType` on the append body;
   the SDK's `defineAggregate('task', events)` stamps it automatically so
   humans never type it per call.
3. **NOT NULL (default '')** — no nullable "unknown" seam. `''` = untyped
   stream, which keeps the field **optional on the wire**: existing callers
   keep working (the contract is additive), and permissive-log philosophy is
   preserved — the server never rejects an untyped append.

The type is **fixed by the stream's first append**: a later append carrying a
different non-empty `streamType` is a 400 (caller bug), and appends omitting
it inherit the existing type. Enforcement costs nothing extra — the type is
read in the same aggregate query as the version check.

Category read: `GET /v1/events?type=<streamType>` — a read-model-free
category query. Fold routing is unchanged (still event-type based);
stream-type-scoped folds can be added later without schema changes.

## Read paging (same change set)

Log reads (`/v1/events`, stream history, by-correlation) previously returned
everything. They now page: cursor (`fromGlobalSeq` / `fromVersion`) +
`limit`, **default 1000, explicit up to 10000**. The generic query endpoint's
defaults moved likewise (100→1000 default, 1000→10000 max). Internal replay
(rebuild) remains unbounded — the limit is an HTTP-surface concern.

## Consequences

- Raw log is self-describing; the "event types must be aggregate-unique"
  naming discipline is no longer load-bearing for forensics.
- One more conformance-locked envelope field; both implementations green.
- Pre-stream_type databases upgrade in place (best-effort `ALTER TABLE` on
  boot; old rows read as untyped `''`).
