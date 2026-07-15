import { describe, it, expect, beforeEach } from 'vitest'
import { createClient } from '@libsql/client'
import { drizzle } from 'drizzle-orm/libsql'
import { createEventStoreRepo, events, SQLITE_SCHEMA_SQL } from '@baseworks/eventstore'
import { Registry, type SqlClient } from '@baseworks/readmodel'
import { buildApp } from './app.js'
import type { Hono } from 'hono'

let app: Hono

const NOTES_DEF = {
  name: 'notes',
  columns: { owner: 'text', text: 'text', created_at: 'integer' },
  on: {
    NoteAdded:   { op: 'upsert', set: { owner: '$.owner', text: '$.text', created_at: '$.createdAt' } },
    NoteDeleted: { op: 'delete' },
  },
}
const NOTES_POLICY = { name: 'notes', role: '*', using: 'owner = :auth_uid' }

function req(method: string, path: string, opts: { body?: unknown; tenant?: string; uid?: string; role?: string } = {}) {
  const headers: Record<string, string> = { 'X-Tenant-ID': opts.tenant ?? 't1' }
  if (opts.uid) headers['X-Auth-UID'] = opts.uid
  if (opts.role) headers['X-Auth-Role'] = opts.role
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json'
  return app.fetch(
    new Request(`http://es${path}`, {
      method,
      headers,
      ...(opts.body !== undefined ? { body: JSON.stringify(opts.body) } : {}),
    }),
  )
}

async function append(streamId: string, expectedVersion: number, type: string, payload: Record<string, unknown>) {
  return req('POST', `/v1/streams/${streamId}`, {
    body: { expectedVersion, events: [{ type, streamId, actor: 'test', payload }] },
  })
}

beforeEach(async () => {
  const client = createClient({ url: ':memory:' })
  await client.executeMultiple(SQLITE_SCHEMA_SQL)
  const db = drizzle(client, { schema: { events } })
  const store = createEventStoreRepo(db, { events })
  const registry = new Registry(client as unknown as SqlClient)
  await registry.init()
  app = buildApp({ client: client as unknown as SqlClient, store, registry, quiet: true })
})

describe('projection registration', () => {
  it('registers a projection + policy, then serves the full flow', async () => {
    expect((await req('PUT', '/v1/projections', { body: NOTES_DEF })).status).toBe(200)
    expect((await req('PUT', '/v1/policies', { body: NOTES_POLICY })).status).toBe(200)

    const a = await append('n1', 0, 'NoteAdded', { owner: 'u1', text: 'hello', createdAt: 111 })
    expect(a.status).toBe(200)
    expect(((await a.json()) as { projected: boolean }).projected).toBe(true)

    const q = await req('POST', '/v1/query/notes', { body: {}, uid: 'u1' })
    expect(q.status).toBe(200)
    const { rows } = (await q.json()) as { rows: Array<Record<string, unknown>> }
    expect(rows).toHaveLength(1)
    expect(rows[0]).toMatchObject({ id: 'n1', owner: 'u1', text: 'hello' })
  })

  it('events first, definition later — registration auto-rebuilds', async () => {
    await req('PUT', '/v1/projections', { body: NOTES_DEF })
    await req('PUT', '/v1/policies', { body: NOTES_POLICY })
    await append('n1', 0, 'NoteAdded', { owner: 'u1', text: 'early', createdAt: 1 })

    // a second projection registered AFTER the event already exists
    const r = await req('PUT', '/v1/projections', {
      body: { name: 'note_stats', columns: { added: 'integer' }, on: { NoteAdded: { op: 'upsert', inc: { added: 1 } } } },
    })
    expect(((await r.json()) as { rebuiltFrom: number }).rebuiltFrom).toBe(1)

    await req('PUT', '/v1/policies', { body: { name: 'note_stats', role: '*' } })
    const q = await req('POST', '/v1/query/note_stats', { body: {}, uid: 'u1' })
    const { rows } = (await q.json()) as { rows: Array<Record<string, unknown>> }
    expect(rows[0]!['added']).toBe(1)
  })

  it('rejects an invalid definition', async () => {
    const r = await req('PUT', '/v1/projections', { body: { name: '_sneaky', columns: {}, on: {} } })
    expect(r.status).toBe(400)
  })
})

describe('query surface', () => {
  beforeEach(async () => {
    await req('PUT', '/v1/projections', { body: NOTES_DEF })
    await req('PUT', '/v1/policies', { body: NOTES_POLICY })
    await append('n1', 0, 'NoteAdded', { owner: 'u1', text: 'alpha', createdAt: 100 })
    await append('n2', 0, 'NoteAdded', { owner: 'u2', text: 'beta', createdAt: 200 })
  })

  it('scopes rows to the caller via the policy', async () => {
    const mine = (await (await req('POST', '/v1/query/notes', { body: {}, uid: 'u1' })).json()) as { rows: unknown[] }
    expect(mine.rows).toHaveLength(1)
    const theirs = (await (await req('POST', '/v1/query/notes', { body: {}, uid: 'u2' })).json()) as { rows: unknown[] }
    expect(theirs.rows).toHaveLength(1)
  })

  it('a delete event removes the row from the projection', async () => {
    await append('n1', 1, 'NoteDeleted', {})
    const q = (await (await req('POST', '/v1/query/notes', { body: {}, uid: 'u1' })).json()) as { rows: unknown[] }
    expect(q.rows).toHaveLength(0)
  })

  it('403s without a matching policy, 404s unknown projections', async () => {
    expect((await req('POST', '/v1/query/notes', { body: {} })).status).toBe(403)   // no uid → policy unsatisfiable
    expect((await req('POST', '/v1/query/nope', { body: {}, uid: 'u1' })).status).toBe(404)
    expect((await req('POST', '/v1/query/_policies', { body: {}, uid: 'u1' })).status).toBe(404)
  })

  it('400s hostile queries', async () => {
    const r = await req('POST', '/v1/query/notes', { body: { select: ['id; DROP TABLE read_notes'] }, uid: 'u1' })
    expect(r.status).toBe(400)
  })

  it('rebuild replays the log for the tenant', async () => {
    const r = await req('POST', '/admin/rebuild', { body: {} })
    expect(r.status).toBe(200)
    expect(((await r.json()) as { rebuiltFrom: number }).rebuiltFrom).toBe(2)
    const q = (await (await req('POST', '/v1/query/notes', { body: {}, uid: 'u1' })).json()) as { rows: unknown[] }
    expect(q.rows).toHaveLength(1)
  })

  it('isolates tenants end to end', async () => {
    const other = (await (await req('POST', '/v1/query/notes', { body: {}, uid: 'u1', tenant: 't2' })).json()) as {
      rows: unknown[]
    }
    expect(other.rows).toHaveLength(0)
  })
})
