# ADR-001 — Contract-first: OpenAPI spec + language-agnostic conformance suite

- **Status**: accepted
- **Date**: 2026-07-15
- **Series**: foldbase (platform ADRs, e.g. ADR-0004 flect bindings, are a separate series)

## Context

The service will become the data-access layer for every project (Supabase/
PostgREST role): each app deploys its own instance, and clients will exist in
multiple languages (TS, Python, more later). The backend itself will be
rewritten in Go (ADR-006). With implementations and clients multiplying, the
TypeScript source can no longer serve as the de-facto contract.

## Decision

1. **`openapi.yaml` is the single source of truth.** Every implementation and
   every client SDK is written against it. Behavior not in the spec is not
   part of the contract.
2. **A language-agnostic conformance suite** exercises the contract over HTTP
   against a running instance. The `examples/taskboard` scenario (register →
   append w/ 409 retry → query w/ policies, roles, tenant isolation → rebuild
   → auth modes, incl. hostile-query cases) is its backbone.
3. Sequence for any rewrite/port: spec → conformance green against the current
   reference implementation → new implementation until conformance green →
   switch. The old implementation retires only after parity.
4. Contract changes are **additive within v1**; anything breaking is a new
   major with its own spec.

## Consequences

- The Go rewrite is de-risked: parity is proven, not assumed.
- Client SDKs in any language have an authoritative target; codegen possible.
- Cost: the spec must be maintained as rigorously as code — a PR that changes
  the HTTP surface without touching `openapi.yaml` is incomplete.
