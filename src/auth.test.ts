import { describe, it, expect, beforeEach, afterEach } from 'vitest'
import { createClient } from '@libsql/client'
import { drizzle } from 'drizzle-orm/libsql'
import { createEventStoreRepo, events, SQLITE_SCHEMA_SQL } from '@baseworks/eventstore'
import { Registry, type SqlClient } from '@baseworks/readmodel'
import { signHs256Jwt } from '@baseworks/auth/jwt'
import { buildApp } from './app.js'
import type { Hono } from 'hono'

const SECRET = 'test-realm-secret-32-chars-long-ok!'
let app: Hono

// A signed, tenant-scoped token the way the broker mints one for a deployed app.
function token(tenant: string): string {
  return signHs256Jwt({ sub: 'app', org_id: tenant, type: 'service' }, SECRET, 3600)
}

function req(method: string, path: string, o: { body?: unknown; auth?: string; tenantHeader?: string } = {}) {
  const headers: Record<string, string> = {}
  if (o.auth) headers['Authorization'] = `Bearer ${o.auth}`
  if (o.tenantHeader) headers['X-Tenant-ID'] = o.tenantHeader
  if (o.body !== undefined) headers['Content-Type'] = 'application/json'
  return app.fetch(
    new Request(`http://es${path}`, { method, headers, ...(o.body !== undefined ? { body: JSON.stringify(o.body) } : {}) }),
  )
}

beforeEach(async () => {
  process.env['FOLDBASE_JWT_SECRET'] = SECRET   // secured mode
  const client = createClient({ url: ':memory:' })
  await client.executeMultiple(SQLITE_SCHEMA_SQL)
  const store = createEventStoreRepo(drizzle(client, { schema: { events } }), { events })
  const registry = new Registry(client as unknown as SqlClient)
  await registry.init()
  app = buildApp({ client: client as unknown as SqlClient, store, registry, quiet: true })
})

afterEach(() => {
  delete process.env['FOLDBASE_JWT_SECRET']
})

describe('secured mode — tenant from the verified token, not X-Tenant-ID', () => {
  it('accepts a valid token and derives the tenant from it', async () => {
    const r = await req('POST', '/v1/streams/s1', {
      auth: token('acme'),
      body: { expectedVersion: 0, events: [{ type: 'Ping', streamId: 's1', actor: 'u', payload: {} }] },
    })
    expect(r.status).toBe(200)
  })

  it('rejects a request with no token (401) even if X-Tenant-ID is set', async () => {
    const r = await req('GET', '/v1/events', { tenantHeader: 'acme' })
    expect(r.status).toBe(401)
  })

  it('rejects a token signed with the wrong secret (401)', async () => {
    const forged = signHs256Jwt({ sub: 'x', org_id: 'acme' }, 'wrong-secret-wrong-secret-wrong!', 3600)
    const r = await req('GET', '/v1/events', { auth: forged })
    expect(r.status).toBe(401)
  })

  it('X-Tenant-ID conflicting with a pinned token org_id is rejected (403, ADR-004)', async () => {
    // token('acme') pins org_id=acme; a mismatching X-Tenant-ID is a hard error,
    // not a silently-ignored header — fail loud on inconsistent intent.
    const r = await req('POST', '/v1/streams/s1', {
      auth: token('acme'),
      tenantHeader: 'evil',
      body: { expectedVersion: 0, events: [{ type: 'Ping', streamId: 's1', actor: 'u', payload: {} }] },
    })
    expect(r.status).toBe(403)
    // nothing leaked into either tenant
    const evil = await req('GET', '/v1/events', { auth: token('evil') })
    expect(((await evil.json()) as unknown[]).length).toBe(0)
    const acme = await req('GET', '/v1/events', { auth: token('acme') })
    expect(((await acme.json()) as unknown[]).length).toBe(0)
  })

  it('a pinned token with no X-Tenant-ID resolves tenant from org_id', async () => {
    const r = await req('POST', '/v1/streams/s1', {
      auth: token('acme'),
      body: { expectedVersion: 0, events: [{ type: 'Ping', streamId: 's1', actor: 'u', payload: {} }] },
    })
    expect(r.status).toBe(200)
    const acme = await req('GET', '/v1/events', { auth: token('acme') })
    expect(((await acme.json()) as unknown[]).length).toBe(1)
  })
})
