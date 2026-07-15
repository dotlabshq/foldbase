# ADR-002 — Explicit auth modes (`FOLDBASE_AUTH`), fail-closed in production

- **Status**: accepted
- **Date**: 2026-07-15

## Context

The service previously inferred its trust boundary: JWT secret present →
verified-token mode, absent → spoofable `X-Tenant-ID` fallback. The failure
mode is silent: deploy externally, forget the secret, and the service runs
open with no warning. As the universal data layer this is unacceptable.

Internal-network (sibling) deployments legitimately need a zero-auth mode;
external exposure must make token verification mandatory.

## Decision

Auth mode is **declared**, never only inferred:

```
FOLDBASE_AUTH = none | service-jwt | user-jwt
```

| Mode | Accepted callers |
|---|---|
| `none` | No token; tenant from `X-Tenant-ID`. Dev / trusted internal networks. |
| `service-jwt` | Only service tokens (`type: "service"`). User tokens rejected outright. |
| `user-jwt` | Superset: service tokens (full) + user tokens (data-plane query, identity from verified claims; `X-Auth-*` headers ignored). |

Fail-closed rules:

1. `service-jwt` / `user-jwt` without a configured secret → **refuse to boot**.
2. `NODE_ENV=production` (or equivalent) with no auth configuration → **refuse
   to boot** unless `FOLDBASE_AUTH=none` is set explicitly. An open service
   in production is an opt-in, never an accident.
3. Unset `FOLDBASE_AUTH` keeps legacy inference for dev convenience only
   (secret → `service-jwt`, else `none`).

User-token claim mapping: tenant = `org_id`, uid = `sub`, plus `role`,
`email` — bound into policy `:auth_*` placeholders.

## Consequences

- Misconfiguration surfaces at boot, not as a breach.
- `user-jwt` unlocks direct browser exposure later without a redesign
  (AGENTS.md's standing caveat is resolved by design).
- HS256 realm secret stays for now; move to asymmetric (EdDSA + `kid`
  rotation) is a planned follow-up, not part of this decision.
