# ADRs — foldbase

Service-scoped decision series. (Platform ADRs — e.g. ADR-0004, flect service
bindings — are a separate series.)

| # | Decision | Status |
|---|---|---|
| [001](./001-contract-first.md) | Contract-first: `openapi.yaml` + language-agnostic conformance suite | accepted |
| [002](./002-auth-modes.md) | Explicit auth modes (`FOLDBASE_AUTH=none\|service-jwt\|user-jwt`), fail-closed in production | accepted |
| [003](./003-plane-separation.md) | Data-plane / control-plane separation via `type: "service"` claim | accepted |
| [004](./004-service-token-tenant-scope.md) | Service token = app identity; tenant per request via `X-Tenant-ID` (required, no default; optional `org_id` pinning) | accepted |
| [005](./005-no-policy-bypass.md) | No policy bypass for services; user tokens query-only in v1 | accepted |
| [006](./006-go-no-orm.md) | Go rewrite; no ORM; SQL-injection defense as explicit invariants | accepted |
| [007](./007-client-sdks.md) | One client package per language (TS, Python); engines live in the service | accepted |
| [008](./008-stream-type.md) | `stream_type`: denormalized, client-supplied, NOT NULL; log reads page (default 1000) | accepted |
| [009](./009-realtime-sse.md) | Realtime over SSE; in-process bus, service-token gated; TS reference frozen | accepted |
| [010](./010-postgres-dialect.md) | PostgreSQL via a thin dialect adapter (placeholder rewrite, type map, RETURNING); clients unchanged | accepted |

Standing context (decided earlier, reaffirmed): **instance-per-app deployment —
no shared central foldbase.** Each app runs its own instance as a private
sibling; ADR-004's safety argument depends on it.
