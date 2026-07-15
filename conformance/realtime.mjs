#!/usr/bin/env node
// Realtime (SSE) conformance — GET /v1/subscribe (ADR-009).
//
// Realtime is a Go-first feature; the TS reference is frozen at the core 58
// checks pending retirement (ADR-009), so this suite targets the Go binary:
//
//   node conformance/realtime.mjs --cmd "./bin/foldbase" --dir ./go
//
// Proves: auth gating, live delivery, catch-up from a cursor, reconnect
// (Last-Event-ID), gap-free ordering, and the ?type= category filter.

import { bootServer, client, makeChecker, openSSE, signJwt } from './harness.mjs'

const SECRET = 'realtime-realm-secret-32-chars-min!'
const svc = () => signJwt({ sub: 'app', type: 'service' }, SECRET)
const usr = (org) => signJwt({ sub: 'u1', type: 'user', org_id: org }, SECRET)

function ev(streamId, ver, type, streamType) {
  return { expectedVersion: ver, streamType, events: [{ type, streamId, actor: 't', payload: { n: ver } }] }
}

function parseArgs() {
  const a = process.argv.slice(2)
  const out = { cmd: './bin/foldbase', dir: './go' }
  for (let i = 0; i < a.length; i++) {
    if (a[i] === '--cmd') out.cmd = a[++i]
    else if (a[i] === '--dir') out.dir = a[++i]
  }
  return out
}

async function main() {
  const { cmd, dir } = parseArgs()
  console.log(`\n▶ realtime conformance: ${cmd}\n`)
  const c = makeChecker('realtime')
  const server = await bootServer({ cmd, dir, env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET } })
  const call = client(server.base)
  const A = { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' }

  try {
    // ── auth gating ──
    const noTok = await openSSE(server.base, '/v1/subscribe', { 'X-Tenant-ID': 'acme' })
    c.status('sse: no token 401', noTok, 401)
    noTok.close()
    // In service-jwt mode a user token is rejected outright (401). The 403
    // capability gate (valid user token, but subscribe needs a service token)
    // lives in user-jwt mode and is covered by the core auth conformance.
    const userTok = await openSSE(server.base, '/v1/subscribe', { Authorization: `Bearer ${usr('acme')}` })
    c.status('sse: user token rejected 401', userTok, 401)
    userTok.close()

    // ── live delivery: subscribe, then append → event arrives ──
    const live = await openSSE(server.base, '/v1/subscribe', { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' })
    c.eq('sse: connect 200', live.status, 200)
    await call('POST', '/v1/streams/s1', { body: ev('s1', 0, 'Alpha', 'thing'), headers: A })
    await live.waitFor(1)
    c.eq('sse: live event delivered', live.events[0]?.json?.type, 'Alpha')
    c.eq('sse: id is globalSeq', live.events[0]?.id, String(live.events[0]?.json?.globalSeq))
    c.eq('sse: event name is the event type', live.events[0]?.event, 'Alpha')
    // a second append arrives too, in order
    await call('POST', '/v1/streams/s1', { body: ev('s1', 1, 'Beta', 'thing'), headers: A })
    await live.waitFor(2)
    c.eq('sse: second live event', live.events[1]?.json?.type, 'Beta')
    const lastSeq = Number(live.events[1].id)
    live.close()

    // ── catch-up: past events replay from the cursor ──
    const catchup = await openSSE(server.base, '/v1/subscribe?fromGlobalSeq=0', { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' })
    await catchup.waitFor(2)
    c.ok('sse: catch-up replays past events', catchup.events.length >= 2 && catchup.events[0].json.type === 'Alpha')
    catchup.close()

    // ── reconnect via Last-Event-ID: only newer events, gap-free ──
    const reconnect = await openSSE(server.base, '/v1/subscribe', {
      Authorization: `Bearer ${svc()}`,
      'X-Tenant-ID': 'acme',
      'Last-Event-ID': String(lastSeq),
    })
    await call('POST', '/v1/streams/s1', { body: ev('s1', 2, 'Gamma', 'thing'), headers: A })
    await reconnect.waitFor(1)
    c.eq('sse: reconnect resumes after cursor', reconnect.events[0]?.json?.type, 'Gamma')
    c.ok('sse: no events at or before the cursor', reconnect.events.every((e) => Number(e.id) > lastSeq))
    reconnect.close()

    // ── category filter: only matching streamType ──
    const filtered = await openSSE(server.base, '/v1/subscribe?type=other', { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'acme' })
    await call('POST', '/v1/streams/s1', { body: ev('s1', 3, 'Delta', 'thing'), headers: A }) // type 'thing' — filtered out
    await call('POST', '/v1/streams/o1', { body: ev('o1', 0, 'Echo', 'other'), headers: A }) // type 'other' — delivered
    await filtered.waitFor(1)
    c.eq('sse: category filter delivers only matching', filtered.events[0]?.json?.type, 'Echo')
    c.ok('sse: filtered stream excluded', filtered.events.every((e) => e.json.streamType === 'other'))
    filtered.close()

    // ── tenant isolation: another tenant sees nothing of acme's ──
    const other = await openSSE(server.base, '/v1/subscribe?fromGlobalSeq=0', { Authorization: `Bearer ${svc()}`, 'X-Tenant-ID': 'globex' })
    await new Promise((r) => setTimeout(r, 300))
    c.eq('sse: tenant isolation (globex empty)', other.events.length, 0)
    other.close()
  } catch (err) {
    c.ok('realtime suite threw', false, String(err.stack || err))
  } finally {
    await server.stop()
  }

  const pass = c.results.filter((r) => r.pass).length
  const fail = c.results.filter((r) => !r.pass)
  for (const r of c.results) console.log(`  ${r.pass ? '✓' : '✗'} ${r.name}${r.pass ? '' : `\n      ${r.detail}`}`)
  console.log(`\n${fail.length === 0 ? '✅' : '❌'} ${pass}/${c.results.length} realtime checks passed\n`)
  process.exit(fail.length === 0 ? 0 : 1)
}

main().catch((e) => { console.error(e); process.exit(1) })
