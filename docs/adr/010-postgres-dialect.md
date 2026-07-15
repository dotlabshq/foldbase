# ADR-010 — PostgreSQL support via a thin dialect adapter

- **Status**: accepted
- **Date**: 2026-07-15

## Context

SQLite/libsql is the default store; a real deployment will want PostgreSQL for
concurrency and scale. The whole point of the HTTP contract is that this swap
is **invisible to clients** — they speak openapi.yaml, not SQL. The engine was
already dialect-leaning (raw parameterized SQL behind an `SQLDB` interface, a
column-identical pg schema in the TS package), so the gap was small.

## Decision

A single `internal/dialect` package isolates the few real differences; the
engine stays dialect-blind (keeps emitting `?` and lowercase column types).

1. **Placeholders** — the engine emits `?`; a rewriting `Conn`/`Tx` wrapper
   converts `?` → `$1, $2, …` for Postgres at the exec boundary. Purely
   positional (values are always bound params; identifiers never are), so no
   query in the engine changes.
2. **Column types** — `integer` maps to **BIGINT** on Postgres (read models
   store epoch-ms timestamps and counters that overflow a 32-bit INTEGER);
   `real` → DOUBLE PRECISION; `text` → TEXT. SQLite keeps INTEGER/REAL/TEXT.
3. **Events DDL** — the one structurally different statement: autoincrement PK
   is `BIGINT GENERATED ALWAYS AS IDENTITY` on Postgres vs
   `INTEGER … AUTOINCREMENT` on SQLite; 64-bit columns are BIGINT.
4. **global_seq retrieval** — SQLite via `LastInsertId()`; Postgres has none,
   so the insert uses `RETURNING global_seq`. Same row, two paths.
5. **Introspection** — `pragma_table_info(?)` on SQLite,
   `information_schema.columns` on Postgres (registry's additive ALTER).
6. **Unique violation** — SQLite reports it in the message; Postgres via
   SQLSTATE 23505 / "duplicate key". One detector covers both.

`ON CONFLICT … DO UPDATE`, `ALTER TABLE ADD COLUMN`, and `excluded.*` are
shared syntax. One genuine portability fix: inc counters qualify the existing
value as `COALESCE(<table>.<col>, 0)` — a bare column in DO UPDATE is
**ambiguous** on Postgres (target row vs `excluded`); the qualified form works
on both.

`DB_URL` gains `postgres://` / `postgresql://` (driver: `pgx/v5/stdlib`);
`:memory:` and `file:` stay SQLite (modernc, CGO-free).

## Verification

The payoff of contract-first (ADR-001): the **same 58 conformance checks** run
against a real Postgres (`FB_DB_URL=postgres://…`, with `FB_DB_RESET` giving
each suite a fresh schema) and the **13 realtime checks** too — all green,
alongside SQLite. Behavioral identity is proven, not assumed.

## Consequences

- Clients are unchanged; the migration is purely server-side.
- Postgres unlocks true concurrency (SQLite's single-writer limit is gone);
  the UNIQUE index still backstops optimistic concurrency → 409.
- Payload/metadata stay TEXT for dialect parity; promoting to `jsonb` (indexed
  JSON queries) is a future, Postgres-only optimization.
- Only the Go implementation gains Postgres; the TS reference is frozen
  (ADR-009) and remains SQLite-only.
