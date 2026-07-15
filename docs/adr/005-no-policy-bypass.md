# ADR-005 — No policy bypass for services; user tokens are query-only in v1

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Supabase's `service_role` bypasses RLS implicitly — its most notorious
foot-gun. Our query engine's core invariant is deny-by-default: no policy →
no rows, for anyone. A service-side implicit bypass would carve the single
exception into that rule.

Separately: the write side has no policy engine yet, so what user tokens may
do on the data plane must be pinned down.

## Decision

1. **No bypass.** Service tokens do not skip select policies. A service
   reading without a forwarded end-user context registers an explicit policy
   at boot — e.g. `{ name: "tasks", role: "service" }` (no `using` = all
   rows) — and queries with `X-Auth-Role: service`. One line of config keeps
   the invariant exception-free: *no policy, no read, no exceptions*.
2. **User tokens are query-only in v1.** Appends and all control-plane calls
   require a service token; a user-token append → 403. End-user writes always
   flow through the owning app, which enforces domain invariants. A write-
   policy design (stream ownership, event-type grants) is future work and out
   of scope for v1.

## Consequences

- A leaked token of either type reads at most what an operator explicitly
  granted its role.
- The `role` dimension of `_policies` does double duty (end-user roles and
  the `service` role) — no new mechanism.
- Apps must remember the one boot-time line when they need raw reads; the
  conformance suite covers the pattern so SDK docs teach it from day one.
