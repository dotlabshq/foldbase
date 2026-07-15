// taskboard-web — the realistic "owning app" pattern: a small web server that
// holds the SERVICE token and talks to a private foldbase sibling, while the
// browser only says who the current user is. The app verified the user; it
// forwards identity via X-Auth-* so the foldbase's owner policy scopes rows.
//
//   node --import tsx server.mjs      → http://localhost:4000
import http from 'node:http'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { z } from 'zod'
import { bootServer, signJwt } from '../../conformance/harness.mjs'
import { FoldBase, FoldBaseError, defineAggregate, defineProjection } from '../../clients/ts/src/index.ts'

const here = dirname(fileURLToPath(import.meta.url))
const SECRET = 'taskboard-web-secret-32-chars-minimum!'
const PORT = Number(process.env.PORT ?? 4000)

// ── the aggregate: stream type 'task' + its event catalog ─────────────────────
const TaskEvents = defineAggregate('task', {
  TaskCreated: z.object({ owner: z.string(), title: z.string().min(1), at: z.number().int() }),
  TaskMoved: z.object({ status: z.enum(['todo', 'doing', 'done']) }),
  TaskDeleted: z.object({}),
})

// ── projections — columns INFERRED from the event field types ─────────────────
const tasks = defineProjection('tasks', TaskEvents, (on) => ({
  TaskCreated: on.TaskCreated.upsert((e) => ({ owner: e.owner, title: e.title, status: 'todo', created_at: e.at })),
  TaskMoved: on.TaskMoved.upsert((e) => ({ status: e.status })),
  TaskDeleted: on.TaskDeleted.delete(),
}))
const stats = defineProjection('board_stats', TaskEvents, (on) => ({
  TaskCreated: on.TaskCreated.inc({ created: 1 }),
}))

// Boot the foldbase sibling (service-jwt mode) — in production this is a
// separate container discovered via EVENTS_SERVICE_URL, not booted here.
// Persist to a file so the tables survive restarts and can be inspected.
const dbPath = resolve(here, 'taskboard.db')
const es = await bootServer({
  cmd: './bin/foldbase',
  dir: resolve(here, '../../go'),
  env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET, DB_URL: 'file:' + dbPath },
})
console.log('sqlite db:', dbPath)
const svc = signJwt({ sub: 'taskboard-web', type: 'service' }, SECRET)
const api = new FoldBase({ baseUrl: es.base, token: svc, tenant: 'acme' })
const write = api.catalog(TaskEvents) // typed, payload-validated emit

console.log('inferred tasks.columns:', JSON.stringify(tasks.def.columns))

// Register definitions on OUR boot (idempotent, like migrations).
await api.putProjection(tasks.def)
await api.putProjection(stats.def)
await api.putPolicy({ name: 'tasks', role: '*', using: 'owner = :auth_uid' })
await api.putPolicy({ name: 'tasks', role: 'admin' }) // sees all rows
await api.putPolicy({ name: 'board_stats', role: '*' })

// seed a couple of tasks so the board isn't empty on first load
async function seed() {
  const evs = await api.readAll()
  if (evs.length > 0) return
  const t1 = TaskEvents.newId() // bare UUIDv7 = the task identity (read-model PK)
  const t2 = TaskEvents.newId()
  await write.emit(t1, 0, 'TaskCreated', { owner: 'alice', title: 'Write the docs', at: Date.now() }, { actor: 'alice' })
  await write.emit(t2, 0, 'TaskCreated', { owner: 'bob', title: 'Review the Go port', at: Date.now() }, { actor: 'bob' })
  await write.emit(t1, 1, 'TaskMoved', { status: 'doing' }, { actor: 'alice' })
}
await seed()

const json = (res, code, body) => { res.writeHead(code, { 'Content-Type': 'application/json' }); res.end(JSON.stringify(body)) }
const readBody = (req) => new Promise((r) => { let b = ''; req.on('data', (c) => (b += c)); req.on('end', () => r(b ? JSON.parse(b) : {})) })

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://localhost:${PORT}`)
  const user = url.searchParams.get('user') || req.headers['x-user'] || 'alice'
  // The web app forwards the (already-verified) end-user identity.
  const asUser = api.withAuth({ uid: user, role: user === 'admin' ? 'admin' : 'member' })
  try {
    if (req.method === 'GET' && url.pathname === '/') { res.writeHead(200, { 'Content-Type': 'text/html' }); return res.end(HTML) }

    if (req.method === 'GET' && url.pathname === '/api/board') {
      const { rows } = await asUser.query('tasks', { sort: ['created_at'] })
      const stats = (await asUser.query('board_stats')).rows.reduce((n, r) => n + (r.created ?? 0), 0)
      return json(res, 200, { tasks: rows, created: stats })
    }
    if (req.method === 'POST' && url.pathname === '/api/tasks') {
      const { title } = await readBody(req)
      const id = TaskEvents.newId() // bare UUIDv7 stream id (aggregate identity)
      // typed emit — payload is validated against TaskCreated's schema
      await write.emit(id, 0, 'TaskCreated', { owner: user, title, at: Date.now() }, { actor: user })
      return json(res, 200, { ok: true })
    }
    if (req.method === 'POST' && url.pathname.startsWith('/api/tasks/') && url.pathname.endsWith('/move')) {
      const id = url.pathname.split('/')[3]
      const { status } = await readBody(req)
      const v = (await api.streamVersion(id)).version
      await write.emit(id, v, 'TaskMoved', { status }, { actor: user })
      return json(res, 200, { ok: true })
    }
    if (req.method === 'DELETE' && url.pathname.startsWith('/api/tasks/')) {
      const id = url.pathname.split('/')[3]
      const v = (await api.streamVersion(id)).version
      await write.emit(id, v, 'TaskDeleted', {}, { actor: user })
      return json(res, 200, { ok: true })
    }
    json(res, 404, { error: 'not_found' })
  } catch (e) {
    if (e instanceof FoldBaseError) return json(res, e.status === 409 ? 409 : 400, { error: e.code, actual: e.actual })
    // Client-side payload validation (zod) rejected the emit before it was sent.
    if (e?.name === 'ZodError') return json(res, 400, { error: 'invalid_payload', message: e.issues?.[0]?.message })
    console.error(e); json(res, 500, { error: 'internal', message: String(e?.message) })
  }
})

server.listen(PORT, () => console.log(`\n▶ taskboard-web on http://localhost:${PORT}  (foldbase sibling: ${es.base})\n`))
process.on('SIGINT', async () => { await es.stop(); process.exit(0) })
process.on('SIGTERM', async () => { await es.stop(); process.exit(0) })

const HTML = /* html */ `<!doctype html><html><head><meta charset="utf-8"><title>taskboard</title>
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  :root { --bg:#0f1115; --card:#1a1d24; --line:#2a2f3a; --fg:#e6e9ef; --mut:#8b93a7; --acc:#6ea8fe; }
  * { box-sizing:border-box; } body { margin:0; font:14px/1.5 ui-sans-serif,system-ui,-apple-system,sans-serif; background:var(--bg); color:var(--fg); }
  header { display:flex; align-items:center; gap:16px; padding:16px 24px; border-bottom:1px solid var(--line); }
  header h1 { font-size:16px; margin:0; letter-spacing:.3px; } header .sub { color:var(--mut); font-size:12px; }
  header .spacer { flex:1; }
  select, input, button { font:inherit; color:var(--fg); background:var(--card); border:1px solid var(--line); border-radius:8px; padding:8px 10px; }
  button { cursor:pointer; } button:hover { border-color:var(--acc); }
  .add { display:flex; gap:8px; padding:16px 24px; } .add input { flex:1; }
  .board { display:grid; grid-template-columns:repeat(3,1fr); gap:16px; padding:0 24px 24px; }
  .col { background:var(--card); border:1px solid var(--line); border-radius:12px; padding:12px; min-height:200px; }
  .col h2 { font-size:12px; text-transform:uppercase; letter-spacing:.6px; color:var(--mut); margin:4px 4px 12px; display:flex; justify-content:space-between; }
  .task { background:#20242e; border:1px solid var(--line); border-radius:10px; padding:10px 12px; margin-bottom:8px; }
  .task .t { font-weight:500; } .task .m { color:var(--mut); font-size:12px; margin-top:2px; display:flex; justify-content:space-between; align-items:center; gap:8px; }
  .task .actions { display:flex; gap:6px; } .task .actions button { padding:3px 7px; font-size:12px; border-radius:6px; }
  .pill { font-size:11px; color:var(--mut); border:1px solid var(--line); border-radius:999px; padding:1px 8px; }
  .empty { color:var(--mut); font-size:12px; padding:8px; text-align:center; }
</style></head><body>
<header>
  <h1>📋 taskboard</h1>
  <span class="sub">over foldbase · owner-policy scoped</span>
  <span class="spacer"></span>
  <span class="pill" id="stat"></span>
  <label class="sub">view as</label>
  <select id="user">
    <option value="alice">alice (member)</option>
    <option value="bob">bob (member)</option>
    <option value="admin">admin (all rows)</option>
  </select>
</header>
<div class="add">
  <input id="title" placeholder="New task title…" autocomplete="off">
  <button onclick="add()">Add task</button>
</div>
<div class="board" id="board"></div>
<script>
  const COLS = [['todo','To do'],['doing','Doing'],['done','Done']]
  const NEXT = { todo:'doing', doing:'done', done:'todo' }
  const $ = (s) => document.querySelector(s)
  const user = () => $('#user').value
  async function api(method, path, body) {
    const r = await fetch(path + (path.includes('?')?'&':'?') + 'user=' + user(), { method, headers: body?{'Content-Type':'application/json'}:{}, body: body?JSON.stringify(body):undefined })
    return r.json()
  }
  async function load() {
    const { tasks, created } = await api('GET','/api/board')
    $('#stat').textContent = created + ' created (board-wide)'
    $('#board').innerHTML = COLS.map(([k,label]) => {
      const items = tasks.filter(t => t.status === k)
      return '<div class="col"><h2>'+label+'<span>'+items.length+'</span></h2>' +
        (items.length? items.map(card).join('') : '<div class="empty">—</div>') + '</div>'
    }).join('')
  }
  function card(t) {
    return '<div class="task"><div class="t">'+esc(t.title)+'</div>' +
      '<div class="m"><span>'+t.owner+'</span><span class="actions">' +
      '<button onclick="move(\\''+t.id+'\\',\\''+NEXT[t.status]+'\\')">→ '+NEXT[t.status]+'</button>' +
      '<button onclick="del(\\''+t.id+'\\')">✕</button></span></div></div>'
  }
  const esc = (s) => s.replace(/[&<>]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;'}[c]))
  async function add() { const t = $('#title').value.trim(); if(!t) return; $('#title').value=''; await api('POST','/api/tasks',{title:t}); load() }
  async function move(id,status){ await api('POST','/api/tasks/'+id+'/move',{status}); load() }
  async function del(id){ await api('DELETE','/api/tasks/'+id); load() }
  $('#user').onchange = load
  $('#title').addEventListener('keydown', e => { if(e.key==='Enter') add() })
  load()
</script></body></html>`
