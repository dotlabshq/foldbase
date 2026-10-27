#!/usr/bin/env node
// Persistence conformance — the log, the registry and the read models survive
// a process restart. Every other suite runs against a fresh database per boot,
// so none of them can see a store that forgets.
//
//   node conformance/persistence.mjs --cmd "./bin/foldbase" --dir ./go
//
// Target database:
//   (default)       a fresh file: SQLite in a temp dir, removed afterwards
//   FB_DB_URL=...   any real store — libsql://<db>.turso.io?authToken=…,
//                   http://sqld:8080, postgres://… — used as-is
//
// Against a real store the suite is non-destructive: it never resets anything,
// and writes only under a tenant unique to this run. The one shared thing it
// touches is the `persist_check` definition, which every run registers
// identically — so its read table stays behind, holding past runs' tenants.
//
// The first server is stopped with SIGKILL, not a graceful shutdown: durability
// has to hold for a crash, which is what a reschedule or an OOM looks like.

import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { randomUUID } from 'node:crypto'

import { bootServer, client, makeChecker } from './harness.mjs'

function parseArgs() {
  const a = process.argv.slice(2)
  const out = { cmd: './bin/foldbase', dir: './go' }
  for (let i = 0; i < a.length; i++) {
    if (a[i] === '--cmd') out.cmd = a[++i]
    else if (a[i] === '--dir') out.dir = a[++i]
  }
  return out
}

// One definition exercising each fold feature that keeps state across events:
// set, a literal inc, a payload inc, and a payload key spanning streams.
const STOCK = {
  name: 'persist_check',
  columns: { sku: 'text', title: 'text', on_hand: 'integer', moves: 'integer', active: 'boolean' },
  on: {
    SkuListed: { op: 'upsert', key: '$.sku', set: { sku: '$.sku', title: '$.title', active: true } },
    StockMoved: { op: 'upsert', key: '$.sku', inc: { on_hand: '$.quantity', moves: 1 } },
  },
}
const POLICY = { name: 'persist_check', role: '*', using: '1 = 1' }

const sortById = (rows) => [...(rows ?? [])].sort((a, b) => (a.id < b.id ? -1 : 1))
const strip = (rows) => sortById(rows).map(({ updated_at, ...r }) => r)

async function main() {
  const { cmd, dir } = parseArgs()
  const external = process.env.FB_DB_URL
  let tmp
  let dbUrl = external
  if (!external) {
    tmp = mkdtempSync(join(tmpdir(), 'foldbase-persist-'))
    dbUrl = `file:${join(tmp, 'foldbase.db')}`
  }
  const label = external ? external.replace(/authToken=[^&]+/, 'authToken=***').replace(/\/\/[^@/]+@/, '//***@') : dbUrl
  console.log(`\n▶ persistence conformance: ${cmd}\n  database: ${label}\n`)

  const c = makeChecker('persistence')
  const tenant = `persist-${randomUUID().slice(0, 8)}`
  const H = { 'X-Tenant-ID': tenant }
  const env = { DB_URL: dbUrl, FOLDBASE_AUTH: 'none' }
  const append = (call, stream, ver, events, streamType) =>
    call('POST', `/v1/streams/${stream}`, {
      headers: H,
      body: { expectedVersion: ver, ...(streamType ? { streamType } : {}), events: events.map((e) => ({ actor: 'persist', ...e })) },
    })
  const query = (call) => call('POST', '/v1/query/persist_check', { headers: H, body: { sort: ['id'] } })

  let server
  try {
    // ── first life: write ────────────────────────────────────────────────────
    server = await bootServer({ cmd, dir, env })
    let call = client(server.base)
    c.status('boot 1: register projection', await call('PUT', '/v1/projections', { headers: H, body: STOCK }), 200)
    c.status('boot 1: register policy', await call('PUT', '/v1/policies', { headers: H, body: POLICY }), 200)

    const listed = await append(call, 'catalog', 0, [
      { type: 'SkuListed', payload: { sku: 'a-1', title: 'Anvil' } },
      { type: 'SkuListed', payload: { sku: 'b-2', title: 'Bolt' } },
    ], 'catalog')
    c.status('boot 1: append to catalog', listed, 200)
    const moved = await append(call, 'order-1', 0, [
      { type: 'StockMoved', payload: { sku: 'a-1', quantity: 10 } },
      { type: 'StockMoved', payload: { sku: 'b-2', quantity: 4 } },
    ], 'order')
    c.status('boot 1: append to order-1', moved, 200)
    c.eq('boot 1: folded', [listed.json?.projected, moved.json?.projected], [true, true])

    const logBefore = (await call('GET', '/v1/events', { headers: H })).json
    const rowsBefore = (await query(call)).json?.rows
    c.eq('boot 1: event count', logBefore?.length, 4)
    c.eq('boot 1: read model', strip(rowsBefore), [
      { active: true, id: 'a-1', moves: 1, on_hand: 10, sku: 'a-1', title: 'Anvil' },
      { active: true, id: 'b-2', moves: 1, on_hand: 4, sku: 'b-2', title: 'Bolt' },
    ])

    await server.stop() // SIGKILL
    server = undefined

    // ── second life: read back, without registering anything ─────────────────
    server = await bootServer({ cmd, dir, env })
    call = client(server.base)

    const health = (await call('GET', '/healthz')).json
    c.ok('boot 2: projection still registered', health?.projections?.includes('persist_check'), JSON.stringify(health))

    const logAfter = (await call('GET', '/v1/events', { headers: H })).json
    c.eq('boot 2: log identical (ids, globalSeq, payloads)', logAfter, logBefore)
    c.eq('boot 2: stream version', (await call('GET', '/v1/streams/order-1/version', { headers: H })).json, { version: 2 })
    c.eq('boot 2: stream type kept', logAfter?.find((e) => e.streamId === 'catalog')?.streamType, 'catalog')

    const q2 = await query(call)
    c.status('boot 2: policy still applies (query 200)', q2, 200)
    c.eq('boot 2: read model identical', sortById(q2.json?.rows), sortById(rowsBefore))

    c.status('boot 2: stale expectedVersion still rejected',
      await append(call, 'order-1', 0, [{ type: 'StockMoved', payload: { sku: 'a-1', quantity: 1 } }]), 409)
    const more = await append(call, 'order-2', 0, [{ type: 'StockMoved', payload: { sku: 'a-1', quantity: -3 } }], 'order')
    c.status('boot 2: append continues', more, 200)
    c.ok('boot 2: globalSeq continues past the old log',
      more.json?.events?.[0]?.globalSeq > Math.max(...(logBefore ?? []).map((e) => e.globalSeq)),
      JSON.stringify(more.json?.events?.[0]))

    const expected = [
      { active: true, id: 'a-1', moves: 2, on_hand: 7, sku: 'a-1', title: 'Anvil' },
      { active: true, id: 'b-2', moves: 1, on_hand: 4, sku: 'b-2', title: 'Bolt' },
    ]
    c.eq('boot 2: inc builds on the persisted total', strip((await query(call)).json?.rows), expected)

    const rebuilt = await call('POST', '/admin/rebuild', { headers: H, body: { name: 'persist_check' } })
    c.status('boot 2: rebuild', rebuilt, 200)
    c.eq('boot 2: rebuild replays the whole log', [rebuilt.json?.rebuiltFrom, rebuilt.json?.skipped], [5, 0])
    c.eq('boot 2: rebuild reproduces the live view', strip((await query(call)).json?.rows), expected)

    await server.stop()
    server = undefined

    // ── third life: the owning app re-registers on boot, as it always does ───
    server = await bootServer({ cmd, dir, env })
    call = client(server.base)
    const again = await call('PUT', '/v1/projections', { headers: H, body: STOCK })
    c.status('boot 3: re-register is accepted', again, 200)
    c.eq('boot 3: re-register refolds the persisted log', again.json?.rebuiltFrom, 5)
    c.eq('boot 3: read model unchanged', strip((await query(call)).json?.rows), expected)
  } catch (err) {
    c.ok('suite ran to completion', false, `${err.message}${server ? `\n${server.log()}` : ''}`)
  } finally {
    if (server) await server.stop()
    if (tmp) rmSync(tmp, { recursive: true, force: true })
  }

  const failed = c.results.filter((r) => !r.pass)
  for (const r of c.results) console.log(`  ${r.pass ? '✓' : '✗'} ${r.name}${r.pass ? '' : `\n      ${r.detail}`}`)
  const total = c.results.length
  console.log(failed.length ? `\n❌ ${failed.length}/${total} persistence checks failed` : `\n✅ ${total}/${total} persistence checks passed`)
  process.exit(failed.length ? 1 : 0)
}

main()
