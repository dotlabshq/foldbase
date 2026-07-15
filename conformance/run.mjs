#!/usr/bin/env node
// Conformance suite for foldbase v1 (openapi.yaml + ADR 001-007).
//
// The behavior lock: any implementation (TS reference, Go, …) must green EVERY
// check here over HTTP. Run against a built server:
//
//   node conformance/run.mjs --cmd "node dist/index.js" --dir .
//   node conformance/run.mjs --cmd "./bin/foldbase"   --dir ./go
//
// Each suite boots a fresh server with a specific auth env, runs an end-to-end
// scenario, and tears it down.

import { bootServer, client, makeChecker, signJwt } from './harness.mjs'

const SECRET = 'conformance-realm-secret-32chars-min!'

// ── shared fixtures ───────────────────────────────────────────────────────────
const NOTES_DEF = {
  name: 'notes',
  columns: { owner: 'text', text: 'text', created_at: 'integer' },
  on: {
    NoteAdded: { op: 'upsert', set: { owner: '$.owner', text: '$.text', created_at: '$.createdAt' } },
    NoteDeleted: { op: 'delete' },
  },
}
const NOTES_POLICY = { name: 'notes', role: '*', using: 'owner = :auth_uid' }
const STATS_DEF = {
  name: 'note_stats',
  columns: { added: 'integer' },
  on: { NoteAdded: { op: 'upsert', inc: { added: 1 } } },
}

const svc = (extra = {}) => signJwt({ sub: 'app', type: 'service', ...extra }, SECRET)
const usr = (org, extra = {}) => signJwt({ sub: 'u1', type: 'user', org_id: org, role: 'member', ...extra }, SECRET)

function appendBody(streamId, expectedVersion, type, payload) {
  return { expectedVersion, events: [{ type, streamId, actor: 'tester', payload }] }
}

// ── SUITE 1: none mode — the current-behavior baseline (mirrors app.test.ts) ──
async function suiteNone(call, c) {
  const T = { 'X-Tenant-ID': 't1' }

  // register projection + policy (control plane open in `none`)
  c.status('none: PUT projection', await call('PUT', '/v1/projections', { body: NOTES_DEF, headers: T }), 200)
  c.status('none: PUT policy', await call('PUT', '/v1/policies', { body: NOTES_POLICY, headers: T }), 200)

  // append → projected:true
  const a = await call('POST', '/v1/streams/n1', { body: appendBody('n1', 0, 'NoteAdded', { owner: 'u1', text: 'hello', createdAt: 111 }), headers: T })
  c.status('none: append', a, 200)
  c.eq('none: projected true', a.json?.projected, true)
  c.eq('none: append version', a.json?.version, 1)

  // query scoped by policy (owner = :auth_uid)
  const q = await call('POST', '/v1/query/notes', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } })
  c.status('none: query', q, 200)
  c.eq('none: query row count', q.json?.rows?.length, 1)
  c.eq('none: query row shape', { id: q.json?.rows?.[0]?.id, owner: q.json?.rows?.[0]?.owner, text: q.json?.rows?.[0]?.text }, { id: 'n1', owner: 'u1', text: 'hello' })

  // events-first: register a NEW projection after events exist → auto-rebuild
  const reg = await call('PUT', '/v1/projections', { body: STATS_DEF, headers: T })
  c.eq('none: auto-rebuild rebuiltFrom', reg.json?.rebuiltFrom, 1)
  await call('PUT', '/v1/policies', { body: { name: 'note_stats', role: '*' }, headers: T })
  const qs = await call('POST', '/v1/query/note_stats', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } })
  c.eq('none: inc counter', qs.json?.rows?.[0]?.added, 1)

  // concurrency conflict → 409 with actual (NOT 500)
  const conflict = await call('POST', '/v1/streams/n1', { body: appendBody('n1', 0, 'NoteAdded', { owner: 'u1', text: 'dup', createdAt: 1 }), headers: T })
  c.status('none: concurrency 409', conflict, 409)
  c.eq('none: conflict error code', conflict.json?.error, 'concurrency_conflict')
  c.eq('none: conflict actual version', conflict.json?.actual, 1)

  // delete event removes the row
  await call('POST', '/v1/streams/n1', { body: appendBody('n1', 1, 'NoteDeleted', {}), headers: T })
  const afterDel = await call('POST', '/v1/query/notes', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } })
  c.eq('none: delete removes row', afterDel.json?.rows?.length, 0)

  // deny-by-default: no uid → policy unsatisfiable → 403
  c.status('none: deny without uid', await call('POST', '/v1/query/notes', { body: {}, headers: T }), 403)
  // unknown projection → 404
  c.status('none: unknown projection 404', await call('POST', '/v1/query/nope', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } }), 404)
  // physical/underscore tables unreachable → 404
  c.status('none: _policies unreachable 404', await call('POST', '/v1/query/_policies', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } }), 404)
  // hostile query (SQL injection attempt in select) → 400
  c.status('none: hostile select 400', await call('POST', '/v1/query/notes', { body: { select: ['id; DROP TABLE read_notes'] }, headers: { ...T, 'X-Auth-UID': 'u1' } }), 400)

  // structured fold: a payload array/object stored via a mode-A upsert is
  // JSON-encoded server-side (the jsonCol counterpart), retrievable as text.
  await call('PUT', '/v1/projections', {
    body: { name: 'docs', columns: { tags: 'text', body: 'text' }, on: { DocSaved: { op: 'upsert', set: { tags: '$.tags', body: '$.body' } } } },
    headers: T,
  })
  await call('PUT', '/v1/policies', { body: { name: 'docs', role: '*' }, headers: T })
  const ds = await call('POST', '/v1/streams/d1', { body: appendBody('d1', 0, 'DocSaved', { tags: ['x', 'y'], body: 'hello' }), headers: T })
  c.eq('none: structured fold projected', ds.json?.projected, true)
  const dq = await call('POST', '/v1/query/docs', { body: {}, headers: { ...T, 'X-Auth-UID': 'u1' } })
  c.eq('none: json array stored as text', dq.json?.rows?.[0]?.tags, '["x","y"]')

  // client-supplied event id: honored verbatim; invalid id → 400
  const cid = '01920000-0000-7000-8000-000000000abc'
  const withId = await call('POST', '/v1/streams/cid1', { body: { expectedVersion: 0, events: [{ id: cid, type: 'NoteAdded', streamId: 'cid1', actor: 't', payload: { owner: 'u1', text: 'x', createdAt: 1 } }] }, headers: T })
  c.eq('none: client-supplied id honored', withId.json?.events?.[0]?.id, cid)
  c.status('none: invalid client id 400', await call('POST', '/v1/streams/cid2', { body: { expectedVersion: 0, events: [{ id: 'not-a-uuid', type: 'NoteAdded', streamId: 'cid2', actor: 't', payload: {} }] }, headers: T }), 400)

  // stream_type: fixed by first append; category filter; conflict → 400
  const typedBody = (sid, ver, type, payload) => ({ streamType: 'note', ...appendBody(sid, ver, type, payload) })
  const st = await call('POST', '/v1/streams/st1', { body: typedBody('st1', 0, 'NoteAdded', { owner: 'u1', text: 's', createdAt: 5 }), headers: T })
  c.status('none: typed append', st, 200)
  c.eq('none: streamType stored on event', st.json?.events?.[0]?.streamType, 'note')
  // category read: only 'note' streams
  const cat = await call('GET', '/v1/events?type=note', { headers: T })
  c.ok('none: category filter matches', Array.isArray(cat.json) && cat.json.length >= 1 && cat.json.every((e) => e.streamType === 'note'))
  const catNone = await call('GET', '/v1/events?type=ghost', { headers: T })
  c.eq('none: category filter empty for unknown type', catNone.json?.length, 0)
  // conflicting type on an existing stream → 400
  c.status('none: streamType conflict 400', await call('POST', '/v1/streams/st1', { body: { streamType: 'other', ...appendBody('st1', 1, 'NoteAdded', { owner: 'u1', text: 'x', createdAt: 6 }) }, headers: T }), 400)
  // untyped appends still work (back-compat) and read limit param honored
  const lim = await call('GET', '/v1/events?limit=1', { headers: T })
  c.eq('none: read limit honored', lim.json?.length, 1)

  // missing tenant → 400
  c.status('none: missing tenant 400', await call('GET', '/v1/events', {}), 400)

  // tenant isolation: t2 sees nothing t1 wrote
  await call('POST', '/v1/streams/a1', { body: appendBody('a1', 0, 'NoteAdded', { owner: 'z', text: 'x', createdAt: 1 }), headers: T })
  const t2 = await call('GET', '/v1/events', { headers: { 'X-Tenant-ID': 't2' } })
  c.eq('none: tenant isolation', t2.json?.length, 0)

  // rebuild replays the whole tenant log
  const rb = await call('POST', '/admin/rebuild', { body: {}, headers: T })
  c.status('none: rebuild', rb, 200)
  c.ok('none: rebuild rebuiltFrom > 0', (rb.json?.rebuiltFrom ?? 0) > 0)

  // stream read + version
  const ver = await call('GET', '/v1/streams/a1/version', { headers: T })
  c.eq('none: stream version', ver.json?.version, 1)
  const hist = await call('GET', '/v1/streams/a1', { headers: T })
  c.eq('none: stream history length', hist.json?.length, 1)
  c.eq('none: stored event has globalSeq', typeof hist.json?.[0]?.globalSeq, 'number')
  c.eq('none: stored event actor preserved', hist.json?.[0]?.actor, 'tester')
  // event id is a UUIDv7 — version nibble (first char of the 3rd group) is '7'
  const eid = hist.json?.[0]?.id ?? ''
  c.ok('none: event id is uuidv7', /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(eid), `id=${eid}`)
}

// ── SUITE 2: service-jwt mode (ADR-002/003/004) ──────────────────────────────
async function suiteServiceJwt(call, c) {
  // no token → 401
  c.status('svc: no token 401', await call('GET', '/v1/events', { headers: { 'X-Tenant-ID': 'acme' } }), 401)
  // wrong secret → 401
  const forged = signJwt({ sub: 'x', type: 'service' }, 'wrong-secret-wrong-secret-wrong!')
  c.status('svc: forged token 401', await call('GET', '/v1/events', { headers: { Authorization: `Bearer ${forged}`, 'X-Tenant-ID': 'acme' } }), 401)
  // user token rejected outright in service-jwt mode → 401
  c.status('svc: user token rejected 401', await call('GET', '/v1/events', { headers: { Authorization: `Bearer ${usr('acme')}`, 'X-Tenant-ID': 'acme' } }), 401)

  // service token WITHOUT X-Tenant-ID → 400 (ADR-004: no default tenant)
  c.status('svc: no tenant header 400', await call('GET', '/v1/events', { headers: { Authorization: `Bearer ${svc()}` } }), 400)

  const A = { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' }
  // control plane with service token → ok
  c.status('svc: control-plane PUT projection', await call('PUT', '/v1/projections', { body: NOTES_DEF, headers: A }), 200)
  c.status('svc: control-plane PUT policy', await call('PUT', '/v1/policies', { body: NOTES_POLICY, headers: A }), 200)
  // append with service token
  const ap = await call('POST', '/v1/streams/n1', { body: appendBody('n1', 0, 'NoteAdded', { owner: 'u1', text: 'hi', createdAt: 1 }), headers: A })
  c.status('svc: append', ap, 200)
  // query with forwarded X-Auth-UID (trusted-caller)
  const q = await call('POST', '/v1/query/notes', { body: {}, headers: { ...A, 'X-Auth-UID': 'u1' } })
  c.eq('svc: forwarded auth query', q.json?.rows?.length, 1)

  // org_id pinning: token pinned to acme, header says evil → 403
  const pinned = { Authorization: `Bearer ${svc({ org_id: 'acme' })}`, 'X-Tenant-ID': 'evil' }
  c.status('svc: pin mismatch 403', await call('GET', '/v1/events', { headers: pinned }), 403)
  // pinned + matching header → ok
  const pinnedOk = { Authorization: `Bearer ${svc({ org_id: 'acme' })}`, 'X-Tenant-ID': 'acme' }
  c.status('svc: pin match ok', await call('GET', '/v1/events', { headers: pinnedOk }), 200)

  // spoof isolation: append under acme while spoofing header is impossible now
  // (header IS the selector for service tokens, so we verify cross-tenant isolation instead)
  const B = { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'other' }
  const otherEvents = await call('GET', '/v1/events', { headers: B })
  c.eq('svc: cross-tenant isolation', otherEvents.json?.length, 0)
}

// ── SUITE 3: user-jwt mode (ADR-003/004/005) ─────────────────────────────────
async function suiteUserJwt(call, c) {
  // bootstrap definitions with a service token (control plane)
  const S = { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' }
  await call('PUT', '/v1/projections', { body: NOTES_DEF, headers: S })
  await call('PUT', '/v1/policies', { body: NOTES_POLICY, headers: S })
  // service also seeds an all-rows policy for its own role (ADR-005: no bypass)
  await call('PUT', '/v1/policies', { body: { name: 'notes', role: 'service' }, headers: S })
  await call('POST', '/v1/streams/n1', { body: appendBody('n1', 0, 'NoteAdded', { owner: 'u1', text: 'mine', createdAt: 1 }), headers: S })
  await call('POST', '/v1/streams/n2', { body: appendBody('n2', 0, 'NoteAdded', { owner: 'u2', text: 'theirs', createdAt: 2 }), headers: S })

  // user token: tenant from org_id, identity from claims — sees only own row
  const U = { Authorization: `Bearer ${usr('acme', { sub: 'u1' })}` }
  const q = await call('POST', '/v1/query/notes', { body: {}, headers: U })
  c.status('user: query ok', q, 200)
  c.eq('user: policy scopes to claim uid', q.json?.rows?.length, 1)
  c.eq('user: sees own row', q.json?.rows?.[0]?.owner, 'u1')

  // X-Auth-UID header is IGNORED for user tokens (spoof attempt to see u2's row)
  const spoof = await call('POST', '/v1/query/notes', { body: {}, headers: { ...U, 'X-Auth-UID': 'u2' } })
  c.eq('user: X-Auth-UID ignored', spoof.json?.rows?.[0]?.owner, 'u1')

  // X-Tenant-ID header is IGNORED for user tokens
  const spoofT = await call('POST', '/v1/query/notes', { body: {}, headers: { ...U, 'X-Tenant-ID': 'other' } })
  c.eq('user: X-Tenant-ID ignored', spoofT.json?.rows?.length, 1)

  // user token append → 403 (query-only in v1, ADR-005)
  c.status('user: append forbidden 403', await call('POST', '/v1/streams/x', { body: appendBody('x', 0, 'NoteAdded', { owner: 'u1', text: 'no', createdAt: 1 }), headers: U }), 403)
  // user token control plane → 403 (ADR-003)
  c.status('user: control-plane forbidden 403', await call('PUT', '/v1/projections', { body: STATS_DEF, headers: U }), 403)

  // service role policy: service reading WITHOUT end-user context sees all rows
  const svcRead = await call('POST', '/v1/query/notes', { body: {}, headers: { ...S, 'X-Auth-Role': 'service' } })
  c.eq('user: service role policy sees all', svcRead.json?.rows?.length, 2)

  // no token → 401
  c.status('user: no token 401', await call('POST', '/v1/query/notes', { body: {} }), 401)
}

// ── SUITE 4: fail-closed boot (ADR-002) ───────────────────────────────────────
// This one is checked by the runner (boot expected to FAIL), see main().

const SUITES = [
  { name: 'none', env: { FOLDBASE_AUTH: 'none' }, run: suiteNone },
  { name: 'service-jwt', env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET }, run: suiteServiceJwt },
  { name: 'user-jwt', env: { FOLDBASE_AUTH: 'user-jwt', FOLDBASE_JWT_SECRET: SECRET }, run: suiteUserJwt },
]

// ── runner ────────────────────────────────────────────────────────────────────
function parseArgs() {
  const a = process.argv.slice(2)
  const out = { cmd: 'node dist/index.js', dir: '.' }
  for (let i = 0; i < a.length; i++) {
    if (a[i] === '--cmd') out.cmd = a[++i]
    else if (a[i] === '--dir') out.dir = a[++i]
  }
  return out
}

async function main() {
  const { cmd, dir } = parseArgs()
  console.log(`\n▶ conformance: ${cmd}  (cwd ${dir})\n`)
  const all = []

  for (const suite of SUITES) {
    let server
    const c = makeChecker(suite.name)
    try {
      server = await bootServer({ cmd, dir, env: suite.env })
    } catch (err) {
      c.ok(`boot (${suite.name})`, false, String(err.message))
      all.push(...c.results)
      continue
    }
    try {
      await suite.run(client(server.base), c)
    } catch (err) {
      c.ok(`suite threw (${suite.name})`, false, String(err.stack || err))
    } finally {
      await server.stop()
    }
    all.push(...c.results)
  }

  // fail-closed: service-jwt with NO secret must refuse to boot (ADR-002)
  {
    const c = makeChecker('fail-closed')
    let booted = false
    try {
      const s = await bootServer({ cmd, dir, env: { FOLDBASE_AUTH: 'service-jwt' } })
      booted = true
      await s.stop()
    } catch {
      /* expected */
    }
    c.ok('fail-closed: service-jwt without secret refuses boot', booted === false, 'server booted despite missing secret')
    all.push(...c.results)
  }

  // report
  const pass = all.filter((r) => r.pass).length
  const fail = all.filter((r) => !r.pass)
  let cur = ''
  for (const r of all) {
    if (r.suite !== cur) {
      cur = r.suite
      console.log(`\n  ── ${cur} ──`)
    }
    console.log(`  ${r.pass ? '✓' : '✗'} ${r.name}${r.pass ? '' : `\n      ${r.detail}`}`)
  }
  console.log(`\n${fail.length === 0 ? '✅' : '❌'} ${pass}/${all.length} checks passed\n`)
  process.exit(fail.length === 0 ? 0 : 1)
}

main().catch((e) => {
  console.error(e)
  process.exit(1)
})
