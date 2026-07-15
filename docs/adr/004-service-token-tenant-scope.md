# ADR-004 — Service tokens carry app identity; tenant selected per request

- **Status**: accepted
- **Date**: 2026-07-15

## Context

Originally the token's `org_id` claim WAS the tenant, for every token type.
But the owning app serves many end-tenants; per-tenant service tokens mean
minting/caching hundreds of tokens for a process that is already inside the
trust boundary — friction without a security gain (the app holds access to
all of its tenants' data by definition).

Parallel: Supabase's `service_role` key acts for any tenant/user; RLS binds
end-user keys only.

## Decision

- **Service token** (`type: "service"`): `sub` = app id. Tenant is selected
  **per request** via `X-Tenant-ID`:
  - Header absent → **400**. No default tenant, ever — tenant choice is
    always explicit.
  - Optional narrowing: if the token carries `org_id`, the tenant is pinned
    to it; a mismatching header → **403**. The broker can thus mint
    single-tenant service tokens for isolated workers using the same
    mechanism.
- **User token**: tenant always = `org_id` claim; `X-Tenant-ID` is silently
  ignored (spoof-proof, matching existing test behavior).
- Trust in the header derives from cryptographic proof of service-hood:
  `X-Tenant-ID` is only read AFTER the `type: "service"` claim is verified.

## Why this is safe *in this architecture*

Blast radius of a leaked service token = all tenants of that instance. The
fixed instance-per-app model (no shared central store) means those are
exactly the tenants the owning app already commands, and each instance has
its own realm secret — cross-app reach is structurally impossible. In a
shared central store this decision would be wrong; the deployment model is
what makes it sound.

## Consequences

- One token per app process; tenant fan-out is a header.
- Audit: the service stamps the verified token `sub` into stored events'
  `metadata` ("which app process wrote this") — `actor` remains the
  caller-supplied end-identity.
