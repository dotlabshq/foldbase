// taskboard-ts — a kanban over foldbase, exercising the WHOLE surface
// with the TS client: projections (upsert/inc/delete), policies (owner + admin
// role), append with optimistic concurrency (409), generic query (where/sort),
// tenant isolation, and rebuild.
//
// Runnable end-to-end: it boots the Go binary itself (service-jwt mode) so you
// can see the real thing. Run:  node --import tsx board.mjs
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { bootServer, signJwt } from '../../conformance/harness.mjs'
import { FoldBase, FoldBaseError } from '../../clients/ts/src/index.ts'

const here = dirname(fileURLToPath(import.meta.url))
const SECRET = 'taskboard-demo-secret-32-chars-minimum!'
const svcToken = signJwt({ sub: 'taskboard-api', type: 'service' }, SECRET)

// ── the read models (definitions are data, pushed on boot like migrations) ────
const TASKS = {
  name: 'tasks',
  columns: { owner: 'text', title: 'text', status: 'text', created_at: 'integer' },
  on: {
    TaskCreated: { op: 'upsert', set: { owner: '$.owner', title: '$.title', status: 'todo', created_at: '$.at' } },
    TaskMoved: { op: 'upsert', set: { status: '$.status' } },
    TaskCompleted: { op: 'upsert', set: { status: 'done' } },
    TaskDeleted: { op: 'delete' },
  },
}
const STATS = {
  name: 'board_stats',
  columns: { created: 'integer', completed: 'integer' },
  on: {
    TaskCreated: { op: 'upsert', inc: { created: 1 } },
    TaskCompleted: { op: 'upsert', inc: { completed: 1 } },
  },
}
// Row access: each member sees their own tasks; an 'admin' sees everything.
const OWNER_POLICY = { name: 'tasks', role: '*', using: 'owner = :auth_uid' }
const ADMIN_POLICY = { name: 'tasks', role: 'admin' } // no `using` = all rows
const STATS_POLICY = { name: 'board_stats', role: '*' } // board-wide counters, shared

const log = (s) => console.log(s)
const show = (title, rows) => {
  log(`\n  ${title}`)
  for (const r of rows) log(`    • [${r.status}] ${r.title}  (owner ${r.owner})`)
  if (rows.length === 0) log('    (none)')
}

const server = await bootServer({
  cmd: './bin/foldbase',
  dir: resolve(here, '../../go'),
  env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET },
})

try {
  // The owning app registers definitions on ITS boot (idempotent).
  const api = new FoldBase({ baseUrl: server.base, token: svcToken, tenant: 'acme' })
  await api.putProjection(TASKS)
  await api.putProjection(STATS)
  await api.putPolicy(OWNER_POLICY)
  await api.putPolicy(ADMIN_POLICY)
  await api.putPolicy(STATS_POLICY)
  log('▶ taskboard — registered projections: tasks, board_stats\n')

  // Alice and Bob create tasks. streamId = task id; expectedVersion starts at 0.
  await api.append('t1', 0, [{ type: 'TaskCreated', streamId: 't1', actor: 'alice', payload: { owner: 'alice', title: 'Write ADRs', at: 1 } }])
  await api.append('t2', 0, [{ type: 'TaskCreated', streamId: 't2', actor: 'alice', payload: { owner: 'alice', title: 'Ship Go port', at: 2 } }])
  await api.append('t3', 0, [{ type: 'TaskCreated', streamId: 't3', actor: 'bob', payload: { owner: 'bob', title: 'Python client', at: 3 } }])

  // Move + complete (each append advances the stream version).
  await api.append('t1', 1, [{ type: 'TaskMoved', streamId: 't1', actor: 'alice', payload: { status: 'doing' } }])
  await api.append('t2', 1, [{ type: 'TaskCompleted', streamId: 't2', actor: 'alice', payload: {} }])

  // Per-user views via the owner policy (forward the end-user identity).
  const alice = api.withAuth({ uid: 'alice' })
  const bob = api.withAuth({ uid: 'bob' })
  show("alice's tasks (sorted)", (await alice.query('tasks', { sort: ['created_at'] })).rows)
  show("bob's tasks", (await bob.query('tasks')).rows)

  // Admin sees the whole board; filter to just what's in progress.
  const admin = api.withAuth({ uid: 'root', role: 'admin' })
  show('admin — everything in "doing"', (await admin.query('tasks', { where: { status: { eq: 'doing' } } })).rows)

  // Board-wide counters (inc projection).
  const stats = (await alice.query('board_stats')).rows
  log(`\n  board_stats: ${JSON.stringify(stats)}`)

  // Optimistic concurrency in action: a stale write is rejected with 409.
  try {
    await api.append('t1', 1, [{ type: 'TaskMoved', streamId: 't1', actor: 'alice', payload: { status: 'done' } }])
  } catch (e) {
    if (e instanceof FoldBaseError && e.status === 409) {
      log(`\n  ⚠ stale write on t1 rejected (409); stream actually at version ${e.actual} — re-read & retry`)
    } else throw e
  }

  // Tenant isolation: another tenant's board is empty (same instance, own token).
  const other = new FoldBase({ baseUrl: server.base, token: svcToken, tenant: 'globex' }).withAuth({ uid: 'alice' })
  log(`\n  globex tenant sees ${(await other.query('tasks')).rows.length} tasks (isolation)`)

  // Read models are disposable — rebuild replays the log deterministically.
  const rb = await api.rebuild()
  log(`\n  rebuilt board from ${rb.rebuiltFrom} events; alice still has ${(await alice.query('tasks')).rows.length} tasks`)

  log('\n✅ taskboard-ts demo complete\n')
} finally {
  await server.stop()
}
