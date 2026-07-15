import { Hono } from 'hono'
import {
  ConcurrencyError,
  NewEventSchema,
  StreamTypeError,
  StreamTypeSchema,
  type EventStoreRepo,
  type StoredEvent,
} from '@baseworks/eventstore'
import { resolveAuth, type AuthCtx } from '../lib/auth.js'

/** Log reads page with a generous default; explicit limits may raise it. */
const DEFAULT_READ_LIMIT = 1000
const MAX_READ_LIMIT = 10000

function readLimit(raw: string | undefined): number {
  const n = raw ? Number(raw) : NaN
  if (!Number.isInteger(n) || n < 1) return DEFAULT_READ_LIMIT
  return Math.min(n, MAX_READ_LIMIT)
}

type Env = {
  Variables: {
    tenant: string
    ctx: AuthCtx
    canWrite: boolean
    canControl: boolean
    subject?: string
  }
}

export interface StreamsOpts {
  /**
   * Called AFTER an append commits — the read-model fold hook. Log-first is the
   * iron rule: the event is already durable when this runs, so a fold failure
   * can only leave a projection stale (repaired by /admin/rebuild), never
   * invent or lose a fact. Failures are reported, not thrown.
   */
  onAppended?: (tenant: string, events: StoredEvent[]) => Promise<void>
}

/** Validate the append envelope. Returns parsed events or an error message. */
function parseAppendBody(
  raw: unknown,
): { events: ReturnType<typeof NewEventSchema.parse>[]; expectedVersion: number; streamType: string } | { error: string } {
  if (raw === null || typeof raw !== 'object') return { error: 'body must be a JSON object' }
  const b = raw as Record<string, unknown>
  if (typeof b['expectedVersion'] !== 'number' || !Number.isInteger(b['expectedVersion']) || b['expectedVersion'] < 0) {
    return { error: 'expectedVersion must be a non-negative integer' }
  }
  if (!Array.isArray(b['events']) || b['events'].length === 0) {
    return { error: 'events must be a non-empty array' }
  }
  const st = StreamTypeSchema.safeParse(b['streamType'] ?? '')
  if (!st.success) return { error: st.error.issues[0]?.message ?? 'invalid streamType' }
  const events = []
  for (const e of b['events']) {
    const parsed = NewEventSchema.safeParse(e)
    if (!parsed.success) return { error: parsed.error.issues[0]?.message ?? 'invalid event' }
    events.push(parsed.data)
  }
  return { events, expectedVersion: b['expectedVersion'], streamType: st.data }
}

/** HTTP surface for the event store — mirrors the EventStoreRepo interface. */
export function streamsRouter(store: EventStoreRepo, opts: StreamsOpts = {}): Hono<Env> {
  const r = new Hono<Env>()

  // Resolve the caller's tenant + capabilities (ADR-002/003/004).
  r.use('*', async (c, next) => {
    const a = resolveAuth(c)
    if ('error' in a) return c.json({ error: a.error }, a.status)
    c.set('tenant', a.tenant)
    c.set('ctx', a.ctx)
    c.set('canWrite', a.canWrite)
    c.set('canControl', a.canControl)
    c.set('subject', a.subject)
    await next()
  })

  // Current version of a stream.
  r.get('/v1/streams/:streamId/version', async (c) => {
    const version = await store.streamVersion(c.get('tenant'), c.req.param('streamId'))
    return c.json({ version })
  })

  // Append to a stream (optimistic concurrency → 409 on conflict).
  r.post('/v1/streams/:streamId', async (c) => {
    // User tokens are query-only in v1 (ADR-005): writes require a service token.
    if (!c.get('canWrite')) return c.json({ error: 'append requires a service token' }, 403)

    const raw = await c.req.json().catch(() => null)
    const parsed = parseAppendBody(raw)
    if ('error' in parsed) {
      return c.json({ error: 'invalid_append', message: parsed.error }, 400)
    }

    const tenant = c.get('tenant')
    const subject = c.get('subject')
    // Stamp the verified writer into metadata for audit (additive, never clobbers).
    const events = subject
      ? parsed.events.map((e) => ({ ...e, metadata: { writtenBy: subject, ...(e.metadata ?? {}) } }))
      : parsed.events

    try {
      const result = await store.append(tenant, c.req.param('streamId'), parsed.expectedVersion, events, {
        streamType: parsed.streamType,
      })
      // Fold AFTER the commit (log-first). A fold failure never fails the
      // append — the fact is durable; the projection heals on rebuild.
      let projected = true
      if (opts.onAppended) {
        try {
          await opts.onAppended(tenant, result.events)
        } catch (err) {
          projected = false
          console.error('[foldbase] projection failed (rebuild will heal):', err)
        }
      }
      return c.json({ ...result, projected })
    } catch (e) {
      if (e instanceof ConcurrencyError) {
        return c.json({ error: 'concurrency_conflict', actual: e.actual }, 409)
      }
      if (e instanceof StreamTypeError) {
        return c.json({ error: 'invalid_append', message: e.message }, 400)
      }
      // UNIQUE(tenant, stream_id, version) backstop under a race — surface as a
      // conflict, not a 500, so callers can re-read and retry (ADR/contract).
      if (isUniqueViolation(e)) {
        const actual = await store.streamVersion(tenant, c.req.param('streamId'))
        return c.json({ error: 'concurrency_conflict', actual }, 409)
      }
      throw e
    }
  })

  // Read a stream's history (paged: fromVersion cursor + limit, default 1000).
  r.get('/v1/streams/:streamId', async (c) => {
    const fromVersion = c.req.query('fromVersion')
    const evs = await store.readStream(c.get('tenant'), c.req.param('streamId'), {
      ...(fromVersion ? { fromVersion: Number(fromVersion) } : {}),
      limit: readLimit(c.req.query('limit')),
    })
    return c.json(evs)
  })

  // Read events correlated to an id (lineage / tracing).
  r.get('/v1/events/by-correlation/:correlationId', async (c) => {
    const evs = await store.readByCorrelation(c.get('tenant'), c.req.param('correlationId'), {
      limit: readLimit(c.req.query('limit')),
    })
    return c.json(evs)
  })

  // Read the tenant log in global order. `type` narrows to one stream category
  // (a read-model-free category query); fromGlobalSeq + limit page the log.
  r.get('/v1/events', async (c) => {
    const fromGlobalSeq = c.req.query('fromGlobalSeq')
    const type = c.req.query('type')
    const evs = await store.readAll(c.get('tenant'), {
      ...(fromGlobalSeq ? { fromGlobalSeq: Number(fromGlobalSeq) } : {}),
      ...(type ? { streamType: type } : {}),
      limit: readLimit(c.req.query('limit')),
    })
    return c.json(evs)
  })

  return r
}

/** Detect a SQLite/libSQL UNIQUE constraint violation across driver shapes. */
function isUniqueViolation(e: unknown): boolean {
  const msg = e instanceof Error ? e.message : String(e)
  return /UNIQUE constraint failed|SQLITE_CONSTRAINT/i.test(msg)
}
