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

/**
 * A read-model column's declared type. `boolean` stores exactly as `integer`
 * (0/1) and differs only on the way back, where the server returns a JSON
 * boolean; an unset column reads back as null, not false.
 */
export type ColType = 'text' | 'integer' | 'real' | 'boolean'

export interface OpRule {
  op: 'upsert' | 'delete'
  /**
   * The row's identity, as a `$.` payload path. Omitted, the row is the stream
   * the event landed on. A key lets a fold maintain a total that spans streams.
   * A key that does not resolve fails the fold (`projected:false`) — there is
   * no safe default, since falling back to the stream id would put two kinds of
   * identity in one table. Within a projection every rule carries a key or none
   * does.
   */
  key?: string
  set?: Record<string, string | number | boolean | null>
  /**
   * Add to a column on every matching event. A number is a literal; a `$.`
   * string is a payload path, so a total can be maintained by the fold rather
   * than by the app. A path the payload does not carry — or a value that is
   * not a number — fails the fold (`projected:false`) rather than counting as
   * zero, because zero is a valid total and could not be told apart from a
   * real one. A field present and explicitly null counts as zero.
   */
  inc?: Record<string, number | `$.${string}`>
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