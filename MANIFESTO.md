# foldbase — Manifesto (Spek Vision)

> The append-only log, its folded views, and the way to query them.

This is the **Vision** section of the foldbase Spek (Open-Spek v0.1). It states
what foldbase is, what it refuses to be, and why it exists. The complete
knowledge lives across three ratified artifacts:

- **Vision** — this file.
- **Design Record** — [`docs/DESIGN.md`](./docs/DESIGN.md) → the locked
  decisions ([`docs/adr/`](./docs/adr/README.md)) + the interface contract
  ([`openapi.yaml`](./openapi.yaml)).
- **Acceptance Criteria** — [`loop/ACCEPTANCE.md`](./loop/ACCEPTANCE.md) → the
  named, machine-checkable gate.

## What foldbase is

A small, self-hostable **data-access layer for event-sourced apps** — the
Supabase/PostgREST role, but the source of truth is an append-only event log,
not mutable tables. Events go in; declarative fold rules (data, not code)
materialize them into read models; one generic, policy-gated endpoint serves
every read. Register your schema on boot, and never write a per-table endpoint
again.

## Why it exists

Every app that adopts event sourcing rebuilds the same plumbing: an append-only
log with optimistic concurrency, projections, a query surface, row policies,
rebuild-from-log, realtime. foldbase makes that plumbing a **deployable
sibling** — one static binary per app — so the app writes zero storage code and
gets a live, queryable, policy-scoped view of its own history for free.

## What it refuses to be

- **Not a shared central database.** No multi-app cluster. Each app deploys its
  own instance as a private sibling; the blast radius of any credential is one
  app's own data (this is what makes several security decisions sound).
- **Not a CRUD database.** The log is append-only and sacred; corrections are
  new events, never rewrites. Read models are disposable derivations.
- **Not a place for domain logic.** Folds are declarative rules
  (`upsert`/`inc`/`delete`). Anything that needs domain reasoning belongs to the
  owning app, never to the service.
- **Not an ORM.** The engine constructs every SQL statement itself; client input
  never reaches an SQL identifier or fragment. Auditability over abstraction.
- **Not browser-facing.** foldbase is an internal sibling. The owning app is the
  trusted caller; it verifies end users and relays to foldbase.

## The three iron rules (never violated)

1. **Log first, always.** The fold runs *after* the append commits. A fold
   failure can only leave a view stale (`projected: false`, repaired by
   rebuild) — it can never invent or lose a fact.
2. **Read models are disposable.** `read_<name>` tables are derived state; the
   log is authoritative. Schema can evolve *after* the events exist.
3. **Deny-by-default, no bypass.** Every query is gated by a select policy;
   tenant isolation is structural (AND-ed into every statement), never dependent
   on policy text — and no caller, not even a service, bypasses it.

## The wedge

Most systems offer "an append-only log" *or* "a query layer" *or* "row
policies." foldbase's wedge is treating **the log, its folded views, and their
policy-gated query surface as one coherent, contract-first service** — where a
language-agnostic conformance suite is the behavioral lock, so the same contract
yields many implementations (Go today, TS reference, any language tomorrow) and
runs identically on SQLite or PostgreSQL. *Source is the contract; code is one
implementation of it.*
