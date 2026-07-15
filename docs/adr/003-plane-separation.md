# ADR-003 — Data-plane / control-plane separation via the `type: "service"` claim

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Previously any tenant-authenticated caller could hit `PUT /v1/projections`,
`PUT /v1/policies` and `/admin/*` — i.e. rewrite definitions and policies.
Definitions are global per instance while data is tenant-scoped; in any
deployment where more than the owning app can reach the service, this lets a
data-plane caller alter row-access rules.

## Decision

Routes are classified into two planes (tagged `x-plane` in the spec):

| Plane | Routes | Requirement (when auth enabled) |
|---|---|---|
| **data** | streams append/read, `/v1/events*`, `/v1/query/:name` | any valid token |
| **control** | `PUT /v1/projections`, `PUT /v1/policies`, `POST /admin/*` | token with `type: "service"` claim |

Mechanism: **claim-based, same realm secret** — no second secret, no separate
admin credential. The platform broker already mints service tokens with
`type: "service"`; end-user tokens never carry it, so in `user-jwt` mode the
control plane is closed to users automatically.

In `none` mode the control plane is open (dev). If `FOLDBASE_ADMIN_TOKEN`
is set, it becomes required for control-plane calls even in `none` mode
(cheap hardening for semi-trusted internal networks).

## Alternatives considered

- Separate static admin secret: second secret to distribute and rotate — more
  moving parts, no additional guarantee.
- Per-route allowlists / network policy only: invisible in the contract,
  breaks the "spec is the truth" rule.

## Consequences

- The owning app's boot registration (which already runs with a broker-minted
  service token) keeps working unchanged.
- Defense-in-depth even in the sibling model: a leaked/stray user token can
  never alter definitions.
