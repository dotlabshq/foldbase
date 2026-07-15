# ADR-006 — Go rewrite; no ORM; SQL-injection defense as explicit invariants

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Every app deploys its own instance, so per-instance footprint multiplies:
node image ~180MB / RSS ~60MB vs a Go static binary ~15MB / RSS ~10MB.
A single binary also kills the current release friction (publishing
`@baseworks/readmodel` to npm before every image build). The codebase is
small (~2–3K lines total to port) — the window for a cheap rewrite is now.

On ORM: the TS code already fights it — the foldbase repo discards
drizzle's type safety (`AnyDB = any`) for dialect-agnosticism, and the
readmodel engine (the security-critical part) is already raw parameterized
SQL. The dynamic surfaces (client-driven query compilation, runtime DDL from
definitions) are workloads no ORM models well.

## Decision

1. **Backend is rewritten in Go**, behind the frozen OpenAPI contract and
   conformance suite (ADR-001). TS service remains the reference
   implementation until conformance parity, then retires.
2. **No ORM.** `database/sql` (+ `pgx` for PostgreSQL, hrana/HTTP client for
   sqld) with hand-written SQL behind one narrow `Store` interface — the Go
   equivalent of today's `SqlClient`.
3. Dialect duality (sqld/SQLite now, PostgreSQL later) is one thin adapter:
   placeholder style (`?` ↔ `$n`), introspection (`PRAGMA` ↔
   `information_schema`), column type map. Required regardless of ORM choice.

## SQL-injection defense (the actual answer to "no ORM = injection risk?")

Layered, each with tests:

1. **Values**: every value is a bound parameter. String concatenation of
   values into SQL is forbidden.
2. **Identifiers** (the part parameters can't cover): client input can never
   reach an identifier position. Names are validated at the schema boundary
   (`^[a-z][a-z0-9_]*$`) and resolved only through the registry; `_` tables
   are structurally unreachable.
3. **Policy `using` fragments**: operator-authored config arriving only over
   the control plane (service token, ADR-003); single-fragment check;
   `:auth_*` placeholders bind as parameters.
4. **Enforcement in Go**: SQL strings are consts or built by the one audited
   query compiler; linters forbid `fmt.Sprintf`/concatenation into SQL
   anywhere else.
5. **Conformance suite carries hostile-input cases** (e.g. `select:
   ["id; DROP TABLE …"]` → 400) so every implementation re-proves the
   property.

An ORM would obscure, not strengthen, these guarantees: the engine's security
model is precisely "we construct all SQL; client text never enters it".

## Consequences

- TS packages after the port: readmodel/foldbase **engines** are absorbed
  into the Go service; the TS **authoring layer** (`defineProjection`,
  `jsonCol`, `fromRow`) and HTTP client move into the TS client SDK
  (ADR-007). drizzle leaves the client dependency chain entirely.
- Dev friction to plan for: pure-Go local SQLite (modernc.org/sqlite or a
  local sqld) to stay CGO-free.
