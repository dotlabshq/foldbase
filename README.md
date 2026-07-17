# foldbase

[![Ask DeepWiki](https://deepwiki.com/badge.svg)](https://deepwiki.com/dotlabshq/foldbase)

**The append-only log, its folded views, and the way to query them.**

foldbase is a small, self-hostable data-access layer: events go into an
append-only log (the truth), declarative **fold rules** materialize them into
queryable read models, and one generic, policy-gated endpoint serves every
read. Think *Supabase/PostgREST for event-sourced apps* — deploy one instance
per app, register your schema on boot, and never write a per-table endpoint
again.

```
emit(event) ──▶ fold (rules as data) ──▶ read_<name> tables ──▶ POST /v1/query/:name
     │                                                                  ▲
     └── events table (append-only truth) ──── POST /admin/rebuild ─────┘
```

**No shared central instance.** Every app deploys its own foldbase as a
private sibling: one static, CGO-free Go binary. Storage is embedded SQLite
(`file:` / `:memory:`), networked **Turso / libsql** (`libsql://` / `http://sqld:8080`),
or **PostgreSQL** (`postgres://`) — the same 58 conformance checks green on all
three; clients never see the difference (ADR-010/012).

## Why

- **Log first, always.** Facts are immutable; corrections are new events.
  A fold failure can only leave a view stale (`projected:false`) — repaired by
  replaying the log. Never invented, never lost.
- **Read models are disposable.** `read_<name>` tables are derived state:
  wipe and rebuild anytime. Your schema can evolve *after* the events exist.
- **Deny-by-default rows.** Select policies (`owner = :auth_uid`) gate every
  query; tenant isolation is structural (AND-ed into every SQL statement),
  never dependent on policy text. No bypass for anyone — not even services.
- **Contract-first.** Behavior is defined by [openapi.yaml](./openapi.yaml)
  and locked by a language-agnostic [conformance suite](./conformance/run.mjs)
  (58 checks) that the Go binary greens. A language-agnostic suite means a future
  second implementation can re-adopt it (ADR-011 retired the original TS reference).

## Try it in 5 minutes — with nothing but curl

```bash
# 1. build & start (dev mode: no auth, in-memory db)
just build-go
DB_URL=:memory: FOLDBASE_AUTH=none PORT=3001 ./go/bin/foldbase &
BASE=http://localhost:3001
```

**Register a read model** (definitions are data — idempotent, like a migration):

```bash
curl -s -X PUT $BASE/v1/projections -H "X-Tenant-ID: demo" -H "Content-Type: application/json" -d '{
  "name": "tasks",
  "columns": { "owner": "text", "title": "text", "status": "text" },
  "on": {
    "TaskCreated": { "op": "upsert", "set": { "owner": "$.owner", "title": "$.title", "status": "todo" } },
    "TaskMoved":   { "op": "upsert", "set": { "status": "$.status" } },
    "TaskDeleted": { "op": "delete" }
  }
}'
# → {"ok":true,"name":"tasks","rebuiltFrom":0}

curl -s -X PUT $BASE/v1/policies -H "X-Tenant-ID: demo" -H "Content-Type: application/json" \
  -d '{ "name": "tasks", "role": "*", "using": "owner = :auth_uid" }'
```

**Append events** (optimistic concurrency: `expectedVersion` starts at 0):

```bash
curl -s -X POST $BASE/v1/streams/task-1 -H "X-Tenant-ID: demo" -H "Content-Type: application/json" -d '{
  "expectedVersion": 0,
  "streamType": "task",
  "events": [{ "type": "TaskCreated", "streamId": "task-1", "actor": "alice",
               "payload": { "owner": "alice", "title": "Try foldbase" } }]
}'
# → { "events":[...], "version":1, "projected":true }

curl -s -X POST $BASE/v1/streams/task-1 -H "X-Tenant-ID: demo" -H "Content-Type: application/json" -d '{
  "expectedVersion": 1,
  "events": [{ "type": "TaskMoved", "streamId": "task-1", "actor": "alice",
               "payload": { "status": "doing" } }]
}'
```

A stale write is rejected — re-read and retry:

```bash
curl -s -X POST $BASE/v1/streams/task-1 -H "X-Tenant-ID: demo" -H "Content-Type: application/json" \
  -d '{ "expectedVersion": 0, "events": [{ "type": "TaskMoved", "streamId": "task-1", "actor": "alice", "payload": { "status": "done" } }] }'
# → 409 { "error":"concurrency_conflict", "actual":2 }
```

**Query the read model** (POST-only, rich JSON filters; the policy scopes rows
to the caller's identity):

```bash
curl -s -X POST $BASE/v1/query/tasks \
  -H "X-Tenant-ID: demo" -H "X-Auth-UID: alice" -H "Content-Type: application/json" \
  -d '{ "where": { "status": { "eq": "doing" } }, "sort": ["-updated_at"], "limit": 10 }'
# → { "rows":[{ "id":"task-1", "owner":"alice", "title":"Try foldbase", "status":"doing", ...}], ... }

# no identity → deny-by-default
curl -s -X POST $BASE/v1/query/tasks -H "X-Tenant-ID: demo" -d '{}'   # → 403
```

**Read the log itself** (global order, category filter, cursor pagination —
default limit 1000):

```bash
curl -s "$BASE/v1/events?type=task&limit=100" -H "X-Tenant-ID: demo"          # one category
curl -s "$BASE/v1/streams/task-1" -H "X-Tenant-ID: demo"                      # one stream's history
curl -s -X POST $BASE/admin/rebuild -H "X-Tenant-ID: demo" -d '{}'            # replay log → rebuild views
```

**Subscribe in realtime** (SSE — see new events the moment they're appended;
open this in one terminal, append in another):

```bash
curl -sN "$BASE/v1/subscribe?type=task" -H "X-Tenant-ID: demo"
# id: 3
# event: TaskCreated
# data: {"id":"019f…","streamId":"task-1","streamType":"task","type":"TaskCreated",...}
#
# : ping                                       ← heartbeat keeps the stream alive
```

Delivery is ordered and gap-free: on reconnect the stream resumes from the last
`id` (globalSeq) via the `Last-Event-ID` header — catch-up then live tail. The
owning app subscribes with a service token and relays to its users (ADR-009).

## Client SDKs

**TypeScript** (`@baseworks/foldbase`) — primary. Typed event schemas drive
everything: payload validation, wire rules, even the `read_` table's columns.

```ts
import { FoldBase, defineAggregate, defineProjection } from '@baseworks/foldbase'
import { z } from 'zod'

// the aggregate: stream type + event payload schemas (single source of truth)
const Tasks = defineAggregate('task', {
  TaskCreated: z.object({ owner: z.string(), title: z.string().min(1) }),
  TaskMoved:   z.object({ status: z.enum(['todo', 'doing', 'done']) }),
  TaskDeleted: z.object({}),
})

// projection: typed accessors compile to wire rules; column types are INFERRED
const tasks = defineProjection('tasks', Tasks, (on) => ({
  TaskCreated: on.TaskCreated.upsert((e) => ({ owner: e.owner, title: e.title, status: 'todo' })),
  TaskMoved:   on.TaskMoved.upsert((e) => ({ status: e.status })),
  TaskDeleted: on.TaskDeleted.delete(),
}))

const fb = new FoldBase({ baseUrl: process.env.EVENTS_SERVICE_URL!, token, tenant: 'acme' })
await fb.putProjection(tasks.def)                       // on boot, like a migration
await fb.putPolicy({ name: 'tasks', role: '*', using: 'owner = :auth_uid' })

const write = fb.catalog(Tasks)                         // typed, validated writes
const id = Tasks.newId()                                // uuidv7 stream id = row PK
await write.emit(id, 0, 'TaskCreated', { owner: 'alice', title: 'Ship it' }, { actor: 'alice' })

const { rows } = await fb.withAuth({ uid: 'alice' })
  .query('tasks', { where: { status: { eq: 'todo' } } })
```

**Python** (`foldbase`). The core client is **stdlib-only, zero dependencies**:

```python
from foldbase import FoldBase

fb = FoldBase(base_url, token=svc_token, tenant="acme")
fb.append("task-1", 0, [{"type": "TaskCreated", "streamId": "task-1",
                         "actor": "alice", "payload": {"owner": "alice", "title": "Ship it"}}],
          stream_type="task")
rows = fb.with_auth(uid="alice").query("tasks")["rows"]
```

The **typed authoring layer** (optional extra — `pip install foldbase[schema]`,
needs pydantic) mirrors the TS one: pydantic models are the source of truth,
columns are inferred, and paths are validated at author time:

```python
from pydantic import BaseModel
from foldbase import FoldBase, define_aggregate, define_projection

class TaskCreated(BaseModel):
    owner: str
    title: str
    at: int
class TaskMoved(BaseModel):
    status: str

Tasks = define_aggregate("task", TaskCreated=TaskCreated, TaskMoved=TaskMoved)

# columns INFERRED from model field types; e.owner compiles to "$.owner"
tasks = define_projection("tasks", Tasks, lambda on: {
    "TaskCreated": on.TaskCreated.upsert(lambda e: {"owner": e.owner, "title": e.title, "status": "todo", "created_at": e.at}),
    "TaskMoved":   on.TaskMoved.upsert(lambda e: {"status": e.status}),
})

fb.put_projection(tasks.definition)              # {"columns": {"owner":"text", "created_at":"integer", ...}}
write = fb.catalog(Tasks)                         # typed, payload-validated emit
write.emit(Tasks.new_id(), 0, "TaskCreated",     # stream_type='task' stamped automatically
           {"owner": "alice", "title": "Ship it", "at": 1}, actor="alice")
```

**Go client** — deliberately deferred: the server is Go, in-process use needs
no client, and cross-service callers are TS/Python today. The same conformance
scenario will validate it when it lands.

## Auth — declared, fail-closed

```
FOLDBASE_AUTH = none | service-jwt | user-jwt
```

| Mode | Who calls | Tenant from |
|---|---|---|
| `none` | trusted internal / dev | `X-Tenant-ID` header |
| `service-jwt` | backend services only (`type:"service"` token) | `X-Tenant-ID` (required; token `org_id` pins it) |
| `user-jwt` | services (full) + end users (query-only) | user token's `org_id` claim |

A secured mode without `FOLDBASE_JWT_SECRET`, or production without an
explicit mode, **refuses to boot**. Control-plane routes (definitions, admin)
always require a service token when auth is on. Full model: [ADR-002…005](./docs/adr/).

## The env contract

| Var | Meaning |
|---|---|
| `PORT` | listen port (default 3001) |
| `DB_URL` | `postgres://…` \| `libsql://…` / `http://sqld:8080` \| `:memory:` \| `file:<path>` (default `:memory:`) |
| `FOLDBASE_AUTH` | `none` \| `service-jwt` \| `user-jwt` |
| `FOLDBASE_JWT_SECRET` | HS256 realm secret (secured modes) |
| `FOLDBASE_ADMIN_TOKEN` | optional control-plane gate in `none` mode |

## Develop & verify

```bash
just gate            # the Spek gate: Go unit + conformance + realtime — the oracle
just build-go        # static binary → go/bin/foldbase
just conformance     # the behavior lock: Go must green all 58 checks
just conformance-pg  # the same 58 checks against a real PostgreSQL
just test-all        # gate + TS & Python client query/subscribe smokes
just authoring-py    # Python typed-authoring test (sets up a pydantic venv)
just dev-web         # taskboard demo UI → http://localhost:4000
```

Layout, decisions, deep-dive: [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) ·
[docs/adr/](./docs/adr/README.md) · executable examples in [examples/](./examples).

## This project is a Spek

foldbase is authored as an [Open-Spek](https://github.com/open-spek) Spek —
durable knowledge from which any implementation can be (re)generated and
verified. The knowledge is complete and ratified:

| Spek section | File |
|---|---|
| **Vision** — what it is, what it refuses to be | [`MANIFESTO.md`](./MANIFESTO.md) |
| **Design Record** — locked decisions + rejected alternatives | [`docs/DESIGN.md`](./docs/DESIGN.md) → [`docs/adr/`](./docs/adr/README.md) + [`openapi.yaml`](./openapi.yaml) |
| **Acceptance Criteria** — machine-checkable, named gate | [`loop/ACCEPTANCE.md`](./loop/ACCEPTANCE.md) |
| **Constraints** — toolchain | [`docs/TOOLCHAIN.md`](./docs/TOOLCHAIN.md) |

The gate (`just gate`) is the verification oracle: it boots the server binary
and asserts the contract over HTTP, so it proves the implementation, in any
language, on SQLite or Postgres. The suite is language-agnostic by construction —
`go/` is the current implementation; a second one could re-adopt the same
contract (ADR-011).

## Deploy (as a sibling in a Flect app)

```toml
[[apps]]
name  = "events"
image = "ghcr.io/dotlabshq/foldbase:<tag>"
port  = 3001                      # private: no public/expose → internal only

[[services]]
binding = "EVENTS"                # host app discovers it as EVENTS_SERVICE_URL
```

The owning app registers its projections + policies on ITS boot (idempotent,
like migrations), then emits events and queries — zero per-table code.
