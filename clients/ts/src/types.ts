// Wire types — mirror openapi.yaml. The contract, in TypeScript.

export interface NewEvent {
  /** Optional client-supplied id (UUID). Absent ⇒ the server mints a v7. */
  id?: string
  type: string
  streamId: string
  actor: string
  payload?: Record<string, unknown>
  causationId?: string
  correlationId?: string
  metadata?: Record<string, unknown>
}

export interface StoredEvent {
  tenant: string
  id: string
  streamId: string
  /** The aggregate's kind ('' for untyped streams); same value stream-wide. */
  streamType: string
  version: number
  globalSeq: number
  type: string
  actor: string
  payload: Record<string, unknown>
  causationId?: string
  correlationId?: string
  metadata: Record<string, unknown>
  recordedAt: number
}

export interface AppendResult {
  events: StoredEvent[]
  version: number
  /** Whether the read-model fold landed; false ⇒ events durable but a projection is stale. */
  projected: boolean
}

export type ColType = 'text' | 'integer' | 'real'

export interface OpRule {
  op: 'upsert' | 'delete'
  set?: Record<string, string | number | boolean | null>
  inc?: Record<string, number>
}

export interface ProjectionDef {
  name: string
  table?: string
  columns: Record<string, ColType>
  on: Record<string, OpRule>
}

export interface PolicyDef {
  name: string
  role: string
  action?: 'select'
  using?: string
  allow?: string[]
  deny?: string[]
}

export type WhereOps = {
  eq?: string | number | boolean | null
  ne?: string | number | boolean | null
  gt?: string | number
  gte?: string | number
  lt?: string | number
  lte?: string | number
  like?: string
  in?: Array<string | number>
}

export type WhereNode =
  | { and: WhereNode[] }
  | { or: WhereNode[] }
  | { [column: string]: WhereOps }

export interface QueryRequest {
  select?: string[]
  where?: WhereNode
  sort?: string[]
  limit?: number
  offset?: number
}

export interface QueryResult<Row = Record<string, unknown>> {
  rows: Row[]
  limit: number
  offset: number
}

/** Thrown on any non-2xx response. `code` is the machine-readable error string. */
export class FoldBaseError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message?: string,
    /** Present on 409 concurrency conflicts — the stream's actual version. */
    readonly actual?: number,
  ) {
    super(message ?? code)
    this.name = 'FoldBaseError'
  }
}