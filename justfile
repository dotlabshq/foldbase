image := "ghcr.io/dotlabshq/foldbase"

# ── build ─────────────────────────────────────────────────────────────────────

# Go implementation — the sole backend (ADR-006, ADR-011). GOWORK=off: standalone module.
build-go:
    cd go && GOWORK=off go build -o bin/foldbase .

# Go-native unit tests (store, auth, query engine).
test-go:
    cd go && GOWORK=off go test ./...

# ── conformance (the behavior lock, ADR-001) ─────────────────────────────────────────────────────────────────

# Run the language-agnostic HTTP conformance suite against the Go binary — the
# machine oracle that locks openapi.yaml (ADR-001).
conformance-go: build-go
    node conformance/run.mjs --cmd "./bin/foldbase" --dir ./go

# Go is the sole implementation (ADR-011); conformance == conformance-go.
conformance: conformance-go

# Conformance against a real Postgres (needs a running instance; set FB_DB_URL
# and FB_DB_RESET). Example with the disposable docker container:
#   just conformance-pg
conformance-pg: build-go
    FB_DB_URL="postgres://postgres:fb@localhost:55432/foldbase" \
    FB_DB_RESET="docker exec fbpg psql -U postgres -d foldbase -c 'DROP SCHEMA public CASCADE; CREATE SCHEMA public;'" \
    node conformance/run.mjs --cmd "./bin/foldbase" --dir ./go

# Realtime (SSE) conformance — Go-first (ADR-009); targets the Go binary.
realtime: build-go
    node conformance/realtime.mjs --cmd "./bin/foldbase" --dir ./go

# ── client smokes (ADR-007) ───────────────────────────────────────────────────

smoke-ts: build-go
    cd clients/ts && node --import tsx test/smoke.mjs

smoke-py: build-go
    cd clients/python && python3 tests/smoke.py

subscribe-ts: build-go
    cd clients/ts && node --import tsx test/subscribe.mjs

subscribe-py: build-go
    cd clients/python && python3 tests/subscribe.py

# Python typed authoring layer test (needs pydantic; sets up a local venv).
authoring-py: build-go
    cd clients/python && python3 -m venv .venv && .venv/bin/pip install -q pydantic && .venv/bin/python tests/authoring.py

# ── examples ──────────────────────────────────────────────────────────────────

demo-ts: build-go
    cd examples/taskboard-ts && node --import tsx board.mjs

demo-py: build-go
    cd examples/taskboard-py && python3 board.py

# Everything: conformance + realtime + both client smokes.
test-all: conformance realtime smoke-ts smoke-py subscribe-ts subscribe-py

# The Spek gate (loop/ACCEPTANCE.md): the single machine oracle that proves the
# behavioral contract. Exit 0 ⇔ the Go binary upholds openapi.yaml.
gate: test-go conformance realtime

# ── dev servers (also in ../../.claude/launch.json) ───────────────────────────

# Go binary, none-mode, local file DB (persists across restarts). → :3001
dev-go: build-go
    DB_URL=file:.dev.db FOLDBASE_AUTH=none ./go/bin/foldbase

# Taskboard UI — boots its own foldbase sibling. → http://localhost:4000
dev-web: build-go
    cd examples/taskboard-web && node --import tsx server.mjs

# ── docker (Go static binary → ghcr; context is the go/ dir) ──────────────────
# The shipped image is the Go implementation (ADR-006/ADR-011, distroless static
# binary). Build for multi-arch with buildx when publishing to ghcr.

build-docker tag="latest":
    docker build --platform linux/amd64 -f go/Dockerfile -t {{image}}:{{tag}} go

push-docker tag="latest":
    docker push {{image}}:{{tag}}

release-docker tag="latest":
    just build-docker {{tag}}
    just push-docker {{tag}}
