# ADR-012 — Turso / libsql (sqld) support as a remote SQLite dialect

- **Status**: accepted
- **Date**: 2026-07-17

## Context

DB_URL supported embedded SQLite (`file:` / `:memory:`, modernc, CGO-free) and
PostgreSQL (`postgres://`, pgx). It did **not** support a networked libsql
server (Turso cloud or a self-hosted `sqld`): modernc.org/sqlite is an embedded
engine — it reads the SQLite file format, it does not speak sqld's Hrana
network protocol — and no libsql client driver was imported. A `sqld` reached
via `http://sqld:8080` therefore fell through to the "unsupported DB_URL" error
and the binary refused to boot. (That is why sqld was removed from compose.)

This closes that gap: a networked libsql gives replicated / edge SQLite without
running Postgres, while keeping the operational feel of SQLite.

## Decision

Add the pure-Go **`github.com/tursodatabase/libsql-client-go/libsql`** driver
(Hrana over WebSocket/HTTP). It is the *remote* client — no CGO — so the static,
CGO-free binary property (ADR-006) is preserved. The CGO embedded-replica driver
(`go-libsql`) is deliberately **not** used.

DB_URL gains the libsql schemes, routed to the driver and to the **SQLite
dialect** — because libsql *is* SQLite end-to-end:

```
libsql:// | wss:// | ws:// | https:// | http://   → libsql driver, SQLite dialect
```

These schemes are unambiguous for a database URL (Postgres uses `postgres://`,
embedded SQLite uses `file:` / `:memory:`), so an `http://` DB_URL can only mean
a sqld HTTP endpoint. Turso cloud: `libsql://<db>.turso.io?authToken=…`;
self-hosted sqld: `http://sqld:8080` / `ws://sqld:8080`. The auth token travels
in the URL (like a Postgres password) and is redacted in logs.

**No dialect work was needed.** libsql speaks the full SQLite dialect over the
wire — verified directly: `LastInsertId()`, `pragma_table_info(?)` (bound param
introspection), and `ON CONFLICT DO UPDATE SET c = COALESCE(t.c,0)+?` all behave
identically. So `dialect.Kind = SQLite` for libsql; the placeholder rewriter,
type map, DDL, and unique detection are the SQLite path unchanged (ADR-010).

## Verification

The payoff of contract-first (ADR-001): the **same 58 conformance checks and 13
realtime checks run green against a real `sqld` server** over `http://`,
alongside `:memory:` SQLite and PostgreSQL. Behavioral identity across all three
back ends is proven, not assumed.

## Consequences

- Three storage back ends behind one HTTP contract: embedded SQLite, networked
  libsql/Turso, PostgreSQL — clients never see the difference.
- Binary size grows (~14 MB → ~22 MB) from the libsql client + websocket deps;
  still fully static and CGO-free.
- Networked libsql and Postgres overlap (both are "a database over the network").
  They coexist: libsql for SQLite-native / edge / Turso deployments, Postgres for
  teams standardized on it. Neither is mandated.
