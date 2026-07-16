# ADR-011 — Retire the TS reference implementation; Go is the sole backend

- **Status**: accepted
- **Date**: 2026-07-16
- **Supersedes**: the "TS reference until parity, then retires" clause of ADR-006

## Context

ADR-006 rewrote the backend in Go and kept the TypeScript service as the
*reference implementation* only until conformance parity, then to retire. That
parity has held for a while: conformance is **58/58 on both impls** plus realtime
13/13 and Postgres-dialect green (ADR-010). The Go binary is the shipped artifact
(`ghcr.io/dotlabshq/foldbase`, distroless static binary).

The TS reference has become a liability, not an asset:

- It still runs on the **retired engines** `@baseworks/eventstore` +
  `@baseworks/readmodel` (+ `@getflect/sdk`, drizzle, libsql). Those are the
  pre-foldbase names ADR-006 folded into the Go binary.
- It **no longer builds against its published dependencies**: the root
  `Dockerfile` (which built the TS server) `npm install`s
  `@baseworks/eventstore@0.1.0`, whose published API drifted behind the workspace
  (`StreamTypeError` export missing) → the image boots to a `SyntaxError`. The TS
  image is unshippable; it was briefly and mistakenly pushed as
  `ghcr.io/dotlabshq/foldbase:0.1.0` and replaced by the Go image.
- No remaining consumer needs these engines. `@baseworks/eventstore` is used only
  by the TS reference and by `@baseworks/readmodel`; `@baseworks/readmodel` is
  used only by the TS reference. The TS **client** (`@baseworks/foldbase`) depends
  on `zod` only — its lone `readmodel` mention is a comment, erased from `dist`.

Keeping a second implementation earned its keep as a cross-check while the Go port
was young; that job is done and locked by the conformance suite itself.

## Decision

1. **Delete the TS reference implementation** from the foldbase repo: `src/**`
   (server, routes, tests, `lib/auth`), the root `Dockerfile` (TS image), and the
   root `tsconfig.json` / `tsup.config.ts` that only built it.
2. **Prune the root `package.json`** of TS-reference-only dependencies
   (`@baseworks/eventstore`, `@baseworks/readmodel`, `@getflect/sdk`, `drizzle-orm`,
   `@libsql/client`, `hono`, `@hono/node-server`, `@baseworks/auth`,
   `@baseworks/config`) and the `build`/`dev`/`start`/`test`/`typecheck` scripts
   that targeted `src/`.
3. **Conformance becomes Go-only.** `just conformance` == `conformance-go`;
   `build-ts` / `conformance-ts` / `dev-ts` are removed. The suite still fully
   locks the contract (openapi.yaml) — against the one implementation that ships.
4. **The Go binary is the sole backend and the only released image.**

## Consequences

- `@baseworks/eventstore` and `@baseworks/readmodel` now have **no consumer** in
  the tree. They are retired (roadmap item 3): deprecate on npm; their source repos
  can be archived. Not deleted from npm (published versions stay for history).
- The contract lock loses its second-implementation cross-check. Mitigation: the
  conformance suite is the oracle regardless of implementation count; a future
  second impl can re-adopt it. Hostile-input and behavioral cases remain (ADR-006).
- Release friction disappears entirely — no npm publish precedes an image build
  (the original motivation in ADR-006).
- Dev loop is Go-only: `just dev-go` (hot reload via rebuild), not `dev-ts`.
