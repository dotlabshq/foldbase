import { Hono } from 'hono'
import type { EventStoreRepo } from '@baseworks/eventstore'
import { resolveAuth, type AuthCtx } from '../lib/auth.js'
import {
  execQuery,
  rebuildProjection,
  rebuildTenant,
  QueryValidationError,
  ReadModelForbiddenError,
  ReadModelNotFoundError,
  type AuthCtx as EngineAuthCtx,
  type Registry,
  type SqlClient,
} from '@baseworks/readmodel'

type Env = {
  Variables: {
    tenant: string
    ctx: AuthCtx
    canWrite: boolean
    canControl: boolean
    subject?: string
  }
}

/**
 * The generic read-model surface (mode A):
 *
 *   PUT  /v1/projections     register/update a projection def (idempotent) → auto-rebuild  [control]
 *   PUT  /v1/policies        register/update a select policy (idempotent)                  [control]
 *   POST /v1/query/:name     JSON query over a registered projection (never GET)           [data]
 *   POST /admin/reload       re-read _projections/_policies into memory                    [control]
 *   POST /admin/rebuild      replay the tenant log into one/all projections                [control]
 *
 * Control-plane routes require a service token (ADR-003). Query auth context is
 * the caller's identity: forwarded X-Auth-* for service tokens, verified claims
 * for user tokens (resolveAuth sets it correctly per token type).
 */
export function readmodelRouter(db: SqlClient, registry: Registry, store: EventStoreRepo): Hono<Env> {
  const r = new Hono<Env>()

  r.use('*', async (c, next) => {
    const a = resolveAuth(c)
    if ('error' in a) return c.json({ error: a.error }, a.status)
    c.set('tenant', a.tenant)
    c.set('ctx', a.ctx)
    c.set('canWrite', a.canWrite)
    c.set('canControl', a.canControl)
    await next()
  })

  const requireControl = (c: { get: (k: 'canControl') => boolean }) => c.get('canControl')

  // Register a projection (control plane). Definitions are DATA authored by the
  // owning app and pushed on ITS boot (like migrations). Registration always
  // ends with a rebuild for the calling tenant, so "events first, definition
  // later" is safe — order between appends and registration never matters.
  r.put('/v1/projections', async (c) => {
    if (!requireControl(c)) return c.json({ error: 'control plane requires a service token' }, 403)
    const tenant = c.get('tenant')
    const def = await registry.saveProjection(await c.req.json())
    const events = await store.readAll(tenant)
    await rebuildProjection(db, registry, def.name, tenant, events)
    return c.json({ ok: true, name: def.name, rebuiltFrom: events.length })
  })

  r.put('/v1/policies', async (c) => {
    if (!requireControl(c)) return c.json({ error: 'control plane requires a service token' }, 403)
    const def = await registry.savePolicy(await c.req.json())
    return c.json({ ok: true, name: def.name, role: def.role })
  })

  // The generic query endpoint (data plane). POST-only: rich nested filters in
  // the body, no URL limits, trivially extensible.
  r.post('/v1/query/:name', async (c) => {
    const ctx = c.get('ctx')
    const engineCtx: EngineAuthCtx = {
      tenant: c.get('tenant'),
      uid: ctx.uid,
      role: ctx.role,
      email: ctx.email,
    }
    const body = await c.req.json().catch(() => ({}))
    const result = await execQuery(db, registry, c.req.param('name'), body, engineCtx)
    return c.json(result)
  })

  r.post('/admin/reload', async (c) => {
    if (!requireControl(c)) return c.json({ error: 'control plane requires a service token' }, 403)
    await registry.reload()
    return c.json({ ok: true, projections: registry.listProjections().map((d) => d.name) })
  })

  // Read models are disposable — replay the log (in globalSeq order) into one
  // projection, or all of them, for the calling tenant.
  r.post('/admin/rebuild', async (c) => {
    if (!requireControl(c)) return c.json({ error: 'control plane requires a service token' }, 403)
    const tenant = c.get('tenant')
    const { name } = (await c.req.json().catch(() => ({}))) as { name?: string }
    const events = await store.readAll(tenant)
    if (name) {
      if (!registry.getProjection(name)) return c.json({ error: 'unknown_projection', name }, 404)
      await rebuildProjection(db, registry, name, tenant, events)
    } else {
      await rebuildTenant(db, registry, tenant, events)
    }
    return c.json({ ok: true, rebuiltFrom: events.length })
  })

  r.onError((err, c) => {
    if (err instanceof ReadModelNotFoundError) return c.json({ error: 'not_found', message: err.message }, 404)
    if (err instanceof ReadModelForbiddenError) return c.json({ error: 'forbidden', message: err.message }, 403)
    if (err instanceof QueryValidationError) return c.json({ error: 'invalid_query', message: err.message }, 400)
    if (err.name === 'ZodError') return c.json({ error: 'invalid_definition', message: err.message }, 400)
    throw err
  })

  return r
}
