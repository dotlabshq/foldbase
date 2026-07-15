# ADR-007 — One client package per language; engines live in the service

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Apps consuming the service need: append (HTTP), query (HTTP), typed
projection authoring + row parsing. Today that spans two TS packages
(`@baseworks/foldbase` — which drags drizzle into installs — and
`@baseworks/readmodel`). The goal has always been that a consuming app stays
minimal; with Python and further languages coming, the app-facing artifact
must be rethought.

## Decision

1. **Per language, exactly one client package**, written against
   `openapi.yaml` (ADR-001) and verified by the same conformance scenario:
   - **TS**: HTTP client + the authoring/typing layer inherited from
     readmodel (`defineProjection`, `jsonCol`, `fromRow`) + zod schemas.
     Dependencies: zod only — no drizzle, no libsql.
   - **Python**: stdlib-only core client (zero deps); the typed authoring
     layer (`define_aggregate`/`define_projection`) is an **optional extra**
     (`foldbase[schema]`) built on pydantic. Because Python type-checking is
     opt-in, the authoring layer adds a **runtime path-validation** guard
     (a captured `e.ownerr` raises at author time) — the Python stand-in for
     TS's compile-time check.
2. Engine code (query compiler, projector, registry, log repo) is **not** a
   client concern — it lives inside the service (Go, after ADR-006).
3. Clients are DB-agnostic by construction (HTTP only): the sqld →
   PostgreSQL migration is invisible to every SDK.
4. Layout: `clients/ts`, `clients/python` beside the service, sharing the
   spec and the conformance scenario (`examples/taskboard`).

## Consequences

- "App reads minimally" is fully realized: a consuming app's dependency is
  one thin package.
- `@baseworks/foldbase` / `@baseworks/readmodel` npm packages wind down
  after the Go port; the authoring layer's code migrates into the TS client.
- A Go client is deliberately deferred: the server is Go (in-process use
  needs no client) and cross-service callers are TS/Python today; the same
  conformance scenario validates it when it lands.
- Each new language costs: client + run the conformance scenario. Nothing
  else.
