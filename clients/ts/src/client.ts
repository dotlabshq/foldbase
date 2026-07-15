import {
  FoldBaseError,
  type AppendResult,
  type NewEvent,
  type PolicyDef,
  type ProjectionDef,
  type QueryRequest,
  type QueryResult,
  type StoredEvent,
} from './types.js'
import type { EventCatalog, EventShapes, PayloadOf } from './schema.js'

/** Build a query string from defined entries only. */
function qs(o: Record<string, string | number | undefined>): string {
  const parts = Object.entries(o)
    .filter(([, v]) => v !== undefined && v !== '')
    .map(([k, v]) => `${k}=${encodeURIComponent(String(v))}`)
  return parts.length ? `?${parts.join('&')}` : ''
}

export interface ClientOptions {
  /** Base URL of the foldbase instance (e.g. EVENTS_SERVICE_URL). */
  baseUrl: string
  /** Bearer token (service or user). Omit in `none`-mode dev. */
  token?: string
  /** X-Tenant-ID — used by service tokens (tenant selector) and `none` mode. */
  tenant?: string
  /** Forwarded end-user identity for a trusted service caller (X-Auth-*). */
  auth?: { uid?: string; role?: string; email?: string }
  /** Injectable fetch (tests / non-global environments). */
  fetch?: typeof fetch
}

/**
 * A thin, typed HTTP client for foldbase. Wire-compatible with any
 * implementation of openapi.yaml (TS reference or Go). Depends on zod only via
 * the authoring layer — the client core is fetch + types.
 *
 *   const es = new FoldBase({ baseUrl, token, tenant: 'acme' })
 *   await es.putProjection(notes.def)
 *   await es.append('n1', 0, [{ type: 'NoteAdded', streamId: 'n1', actor: 'u1', payload }])
 *   const { rows } = await es.query('notes', { where: { owner: { eq: 'u1' } } })
 */
export class FoldBase {
  private readonly baseUrl: string
  private readonly fetchImpl: typeof fetch
  constructor(private readonly opts: ClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/$/, '')
    this.fetchImpl = opts.fetch ?? globalThis.fetch
  }

  /** Return a new client with a different forwarded end-user identity. */
  withAuth(auth: { uid?: string; role?: string; email?: string }): FoldBase {
    return new FoldBase({ ...this.opts, auth })
  }

  /** Return a new client scoped to a different tenant (service tokens / none mode). */
  withTenant(tenant: string): FoldBase {
    return new FoldBase({ ...this.opts, tenant })
  }

  private headers(json: boolean): Record<string, string> {
    const h: Record<string, string> = {}
    if (json) h['Content-Type'] = 'application/json'
    if (this.opts.token) h['Authorization'] = `Bearer ${this.opts.token}`
    if (this.opts.tenant) h['X-Tenant-ID'] = this.opts.tenant
    if (this.opts.auth?.uid) h['X-Auth-UID'] = this.opts.auth.uid
    if (this.opts.auth?.role) h['X-Auth-Role'] = this.opts.auth.role
    if (this.opts.auth?.email) h['X-Auth-Email'] = this.opts.auth.email
    return h
  }

  private async call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const res = await this.fetchImpl(`${this.baseUrl}${path}`, {
      method,
      headers: this.headers(body !== undefined),
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    })
    const text = await res.text()
    const data = text ? (JSON.parse(text) as unknown) : undefined
    if (!res.ok) {
      const e = (data ?? {}) as { error?: string; message?: string; actual?: number }
      throw new FoldBaseError(res.status, e.error ?? 'error', e.message, e.actual)
    }
    return data as T
  }

  // ── streams (data plane) ────────────────────────────────────────────────────

  /** Append events with optimistic concurrency. Throws FoldBaseError(409) on conflict. */
  append(
    streamId: string,
    expectedVersion: number,
    events: NewEvent[],
    opts: { streamType?: string } = {},
  ): Promise<AppendResult> {
    return this.call('POST', `/v1/streams/${encodeURIComponent(streamId)}`, {
      expectedVersion,
      events,
      ...(opts.streamType ? { streamType: opts.streamType } : {}),
    })
  }

  streamVersion(streamId: string): Promise<{ version: number }> {
    return this.call('GET', `/v1/streams/${encodeURIComponent(streamId)}/version`)
  }

  readStream(streamId: string, opts: { fromVersion?: number; limit?: number } | number = {}): Promise<StoredEvent[]> {
    const o = typeof opts === 'number' ? { fromVersion: opts } : opts
    return this.call('GET', `/v1/streams/${encodeURIComponent(streamId)}${qs(o)}`)
  }

  /** Read the tenant log in global order. `type` narrows to one stream category. */
  readAll(opts: { fromGlobalSeq?: number; limit?: number; type?: string } | number = {}): Promise<StoredEvent[]> {
    const o = typeof opts === 'number' ? { fromGlobalSeq: opts } : opts
    return this.call('GET', `/v1/events${qs(o)}`)
  }

  readByCorrelation(correlationId: string, opts: { limit?: number } = {}): Promise<StoredEvent[]> {
    return this.call('GET', `/v1/events/by-correlation/${encodeURIComponent(correlationId)}${qs(opts)}`)
  }

  /** Query a registered read model. */
  query<Row = Record<string, unknown>>(name: string, request: QueryRequest = {}): Promise<QueryResult<Row>> {
    return this.call('POST', `/v1/query/${encodeURIComponent(name)}`, request)
  }

  // ── definitions + admin (control plane; needs a service token) ──────────────

  putProjection(def: ProjectionDef): Promise<{ ok: true; name: string; rebuiltFrom: number }> {
    return this.call('PUT', '/v1/projections', def)
  }

  putPolicy(def: PolicyDef): Promise<{ ok: true; name: string; role: string }> {
    return this.call('PUT', '/v1/policies', def)
  }

  rebuild(name?: string): Promise<{ ok: true; rebuiltFrom: number }> {
    return this.call('POST', '/admin/rebuild', name ? { name } : {})
  }

  reload(): Promise<{ ok: true; projections: string[] }> {
    return this.call('POST', '/admin/reload', {})
  }

  health(): Promise<{ ok: boolean; service: string; projections: string[] }> {
    return this.call('GET', '/healthz')
  }

  /**
   * Subscribe to appended events over SSE (realtime). `onEvent` fires for each
   * event as it is appended; delivery is ordered and gap-free, resuming from the
   * last seen globalSeq across automatic reconnects. Returns a handle — call
   * `close()` to stop. Requires a service token (the raw log bypasses row
   * policies; the app relays to its own users).
   *
   *   const sub = fb.subscribe({ type: 'task' }, (e) => render(e))
   *   // …later
   *   sub.close()
   */
  subscribe(
    opts: { type?: string; fromGlobalSeq?: number },
    onEvent: (event: StoredEvent) => void,
    onError?: (err: unknown) => void,
  ): { close: () => void } {
    let lastId = opts.fromGlobalSeq
    let closed = false
    let ctrl: AbortController | null = null

    const connect = async () => {
      while (!closed) {
        ctrl = new AbortController()
        try {
          const headers = this.headers(false)
          if (lastId !== undefined) headers['Last-Event-ID'] = String(lastId)
          const q = qs({ type: opts.type, fromGlobalSeq: lastId === undefined ? opts.fromGlobalSeq : undefined })
          const res = await this.fetchImpl(`${this.baseUrl}/v1/subscribe${q}`, { headers, signal: ctrl.signal })
          if (!res.ok || !res.body) throw new FoldBaseError(res.status, 'subscribe_failed')
          const reader = res.body.getReader()
          const dec = new TextDecoder()
          let buf = ''
          for (;;) {
            const { value, done } = await reader.read()
            if (done) break
            buf += dec.decode(value, { stream: true })
            let idx: number
            while ((idx = buf.indexOf('\n\n')) >= 0) {
              const raw = buf.slice(0, idx)
              buf = buf.slice(idx + 2)
              if (raw.startsWith(':')) continue // heartbeat
              let id: string | undefined
              let data: string | undefined
              for (const line of raw.split('\n')) {
                if (line.startsWith('id:')) id = line.slice(3).trim()
                else if (line.startsWith('data:')) data = line.slice(5).replace(/^ /, '')
              }
              if (data !== undefined) {
                if (id !== undefined) lastId = Number(id)
                onEvent(JSON.parse(data) as StoredEvent)
              }
            }
          }
        } catch (err) {
          if (closed) return
          onError?.(err)
        }
        if (closed) return
        await new Promise((r) => setTimeout(r, 1000)) // backoff, then reconnect from lastId
      }
    }
    void connect()
    return {
      close: () => {
        closed = true
        ctrl?.abort()
      },
    }
  }

  /** Bind an event catalog for typed, payload-validated writes (`emit`). */
  catalog<S extends EventShapes>(cat: EventCatalog<S>): TypedClient<S> {
    return new TypedClient<S>(this, cat)
  }
}

/** Options for a typed emit — id/actor/causation/correlation/metadata. */
export interface EmitOpts {
  actor?: string
  id?: string
  causationId?: string
  correlationId?: string
  metadata?: Record<string, unknown>
}

/**
 * A catalog-bound wrapper over FoldBase. `emit` type-checks the payload
 * against the event's schema and validates it client-side before appending —
 * the event types are pinned by the catalog, so a wrong type or a malformed
 * payload is a compile error, then a runtime guard.
 */
export class TypedClient<S extends EventShapes> {
  constructor(
    private readonly es: FoldBase,
    private readonly cat: EventCatalog<S>,
  ) {}

  /** Append one typed event. `payload` is checked against the event's schema. */
  emit<K extends keyof S & string>(
    streamId: string,
    expectedVersion: number,
    type: K,
    payload: PayloadOf<S, K>,
    opts: EmitOpts = {},
  ): Promise<AppendResult> {
    const parsed = this.cat.schemas[type].parse(payload) as Record<string, unknown>
    const event: NewEvent = {
      type,
      streamId,
      actor: opts.actor ?? 'system',
      payload: parsed,
      ...(opts.id ? { id: opts.id } : {}),
      ...(opts.causationId ? { causationId: opts.causationId } : {}),
      ...(opts.correlationId ? { correlationId: opts.correlationId } : {}),
      ...(opts.metadata ? { metadata: opts.metadata } : {}),
    }
    // A defineAggregate catalog stamps the stream's type on every append.
    return this.es.append(streamId, expectedVersion, [event], {
      ...(this.cat.streamType ? { streamType: this.cat.streamType } : {}),
    })
  }

  /** The underlying client, for queries / definitions / non-typed calls. */
  get client(): FoldBase {
    return this.es
  }
}
