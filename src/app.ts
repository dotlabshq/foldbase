import { Hono } from 'hono'
import { logger } from 'hono/logger'
import type { EventStoreRepo } from '@baseworks/eventstore'
import { applyEvent, Registry, type SqlClient } from '@baseworks/readmodel'
import { streamsRouter } from './routes/streams.js'
import { readmodelRouter } from './routes/readmodel.js'

export interface AppDeps {
  client: SqlClient
  store: EventStoreRepo
  registry: Registry
  /** Disable request logging (tests). */
  quiet?: boolean
}

/**
 * The service = the append-only log + its materialized views + their generic
 * query surface. Appends fold inline through the mode-A rules engine
 * (@baseworks/readmodel); definitions and policies are data registered by the
 * owning app on ITS boot.
 */
export function buildApp(deps: AppDeps): Hono {
  const app = new Hono()
  if (!deps.quiet) app.use('*', logger())
  app.get('/healthz', (c) =>
    c.json({ ok: true, service: 'foldbase', projections: deps.registry.listProjections().map((d) => d.name) }),
  )

  app.route(
    '/',
    streamsRouter(deps.store, {
      onAppended: async (_tenant, events) => {
        for (const event of events) await applyEvent(deps.client, deps.registry, event)
      },
    }),
  )
  app.route('/', readmodelRouter(deps.client, deps.registry, deps.store))

  app.onError((err, c) => {
    console.error('[foldbase]', err)
    // Don't leak internals in production; keep detail in dev/test.
    const body =
      process.env['NODE_ENV'] === 'production'
        ? { error: 'internal_server_error' }
        : { error: 'internal_server_error', message: err.message }
    return c.json(body, 500)
  })

  return app
}
