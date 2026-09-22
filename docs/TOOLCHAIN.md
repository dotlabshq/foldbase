# foldbase — Toolchain / Constraints (Spek)

The **Constraints** section of the foldbase Spek: the build environment and the
tools the gate depends on. These are locked so any implementer (human or agent
loop) reproduces the same verification.

## Runtimes

| Tool | Version | Used for |
|---|---|---|
| Go | 1.26+ | production implementation (`go/`), unit tests, the static binary |
| Node | 20+ (22 recommended) | TS client (`clients/ts`), the conformance runner (`conformance/*.mjs`) |
| Python | 3.9+ | Python client core (stdlib only); authoring extra needs pydantic ≥ 2 |
| just | any | task runner (defines the gate) |
| Docker | any | `just build-docker`; disposable PostgreSQL for `conformance-pg` (optional) |
| Docker Buildx | multi-platform builder | `just release-docker` only — see below |

`just release-docker` builds `linux/amd64` and `linux/arm64` into one manifest,
which needs a Buildx builder that can target more than one platform: the
containerd image store, or a `docker-container` builder (`docker buildx create
--use`). The classic image store with the default `docker` driver refuses a
multi-platform build. `just build-docker` has no such requirement — it builds
one image for the machine it runs on.

## Dependencies (deliberately minimal)

- **Go:** `modernc.org/sqlite` (pure-Go, CGO-free), `github.com/jackc/pgx/v5`
  (PostgreSQL), `github.com/google/uuid`. No web framework, no ORM.
- **TS client (`@baseworks/foldbase`):** `zod` only — no drizzle, no libsql.
- **Python client (`foldbase`):** stdlib only; `pydantic` optional (`[schema]`).

## The gate depends on

`just gate` = `just test-go` + `just conformance` + `just realtime`. It requires
a built Go binary (`just build-go`, which every gate recipe depends on) and Node
on PATH. `just build-ts` is gone — ADR-011 retired the TS reference. The conformance runner boots each server as a subprocess and
drives it over HTTP; it needs no database service (SQLite `:memory:`). The
PostgreSQL extension (`just conformance-pg`) additionally needs a reachable
Postgres and `FB_DB_URL` / `FB_DB_RESET`.

## Environment contract

`PORT` · `DB_URL` (`postgres://` \| `libsql://`/`http://` \| `:memory:` \| `file:`) · `FOLDBASE_AUTH`
(`none`\|`service-jwt`\|`user-jwt`) · `FOLDBASE_JWT_SECRET` ·
`FOLDBASE_ADMIN_TOKEN`. Every implementation honors these identically — it is
part of the contract the gate enforces.

## Non-negotiable build constraints

- The Go binary stays **CGO-free** (static, ~14 MB) — pure-Go SQLite, no libc.
- The engine emits **`?`-placeholder SQL and lowercase column types**; dialect
  differences live only in `go/internal/dialect` (ADR-010).
- No code path may place client input in an SQL identifier or fragment (ADR-006).
