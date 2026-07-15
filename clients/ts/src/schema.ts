import { z } from 'zod'
import type { ColType, OpRule, ProjectionDef } from './types.js'
import { uuidv7 } from './id.js'

const identifierRe = /^[a-z][a-z0-9_]*$/

/**
 * The typed authoring layer (Approach B — proxy path capture).
 *
 * Event payload schemas are the single source of truth. A projection's field
 * mappings are written as typed accessors (`e => ({ owner: e.owner })`) captured
 * by a proxy that compiles `e.owner` down to the wire path `"$.owner"`. Read-
 * model column types are INFERRED from the event field types, so the event
 * schema drives the `read_<name>` table. The compiled output is exactly the
 * plain wire ProjectionDef — the server contract is unchanged.
 */

// ── event catalog ─────────────────────────────────────────────────────────────

export type EventShapes = Record<string, z.ZodObject<z.ZodRawShape>>

export interface EventCatalog<S extends EventShapes> {
  schemas: S
  /** The aggregate's kind — stamped as stream_type on every append (via emit). */
  streamType?: string
}

/** Declare the events that may enter the log — the source of truth for payloads. */
export function defineEvents<S extends EventShapes>(schemas: S): EventCatalog<S> {
  return { schemas }
}

/**
 * Declare an aggregate: its stream type + its event catalog. `emit` through a
 * catalog-bound client stamps `streamType` automatically, and `newId()` mints
 * the aggregate's stream id (a bare UUIDv7 — the read-model PK).
 */
export function defineAggregate<S extends EventShapes>(
  streamType: string,
  schemas: S,
): EventCatalog<S> & { newId(): string } {
  if (!identifierRe.test(streamType)) {
    throw new Error(`foldbase: stream type '${streamType}' must be a lowercase identifier`)
  }
  return {
    schemas,
    streamType,
    newId: () => uuidv7(),
  }
}

export type PayloadOf<S extends EventShapes, K extends keyof S> = z.infer<S[K]>

// ── zod → column type (shared with the runtime authoring layer) ───────────────

function typeName(s: z.ZodTypeAny): string {
  return (s._def as { typeName?: string }).typeName ?? ''
}
function unwrap(s: z.ZodTypeAny): z.ZodTypeAny {
  const t = typeName(s)
  if (t === 'ZodOptional' || t === 'ZodNullable' || t === 'ZodDefault') {
    return unwrap((s._def as { innerType: z.ZodTypeAny }).innerType)
  }
  return s
}
function colTypeOfZod(field: z.ZodTypeAny): ColType {
  const base = unwrap(field)
  switch (typeName(base)) {
    case 'ZodString':
    case 'ZodEnum':
    case 'ZodNativeEnum':
      return 'text'
    case 'ZodBoolean':
      return 'integer'
    case 'ZodNumber': {
      const checks = (base._def as { checks?: Array<{ kind: string }> }).checks ?? []
      return checks.some((c) => c.kind === 'int') ? 'integer' : 'real'
    }
    default:
      return 'text' // objects/arrays fold to JSON text (jsonCol counterpart)
  }
}

// ── proxy path capture ────────────────────────────────────────────────────────

const PATH = Symbol('foldbase.path')

/** A proxy that records field access as a "$.a.b" payload path. */
function pathProxy(path: string): unknown {
  return new Proxy(
    function () {},
    {
      get(_t, key) {
        if (key === PATH) return path
        if (typeof key === 'symbol') return undefined
        return pathProxy(path === '' ? '$.' + String(key) : path + '.' + String(key))
      },
    },
  )
}
function pathOf(v: unknown): string | undefined {
  if (v && (typeof v === 'object' || typeof v === 'function')) {
    const p = (v as Record<symbol, unknown>)[PATH]
    return typeof p === 'string' ? p : undefined
  }
  return undefined
}

// ── rule builders ─────────────────────────────────────────────────────────────

type SetMap = Record<string, string | number | boolean | null>
interface RawRule {
  op: 'upsert' | 'delete'
  set?: Record<string, unknown> // column → path-marker | literal
  inc?: Record<string, number>
}

export interface RuleBuilder<P> {
  /** Merge columns into the row keyed by (tenant, streamId). */
  upsert(fn: (e: P) => Record<string, string | number | boolean | null>, opts?: { inc?: Record<string, number> }): RawRule
  /** Increment numeric counters only. */
  inc(counters: Record<string, number>): RawRule
  /** Remove the row. */
  delete(): RawRule
}

function ruleBuilder<P>(): RuleBuilder<P> {
  return {
    upsert(fn, opts) {
      const set = fn(pathProxy('') as P) as Record<string, unknown>
      return { op: 'upsert', set, ...(opts?.inc ? { inc: opts.inc } : {}) }
    },
    inc(counters) {
      return { op: 'upsert', inc: counters }
    },
    delete() {
      return { op: 'delete' }
    },
  }
}

// ── defineProjection ──────────────────────────────────────────────────────────

export interface ProjectionSpec {
  /** The plain wire ProjectionDef — exactly what the server persists & queries. */
  def: ProjectionDef
}

type RuleMap<S extends EventShapes> = Partial<{ [K in keyof S]: RawRule }>
type OnHelper<S extends EventShapes> = { [K in keyof S]: RuleBuilder<PayloadOf<S, K>> }

function inferColumn(columns: Record<string, ColType>, col: string, type: ColType): void {
  const existing = columns[col]
  if (existing && existing !== type) {
    throw new Error(`foldbase: column '${col}' inferred as both '${existing}' and '${type}' — declare it explicitly`)
  }
  columns[col] = type
}

/**
 * Build a projection from an event catalog. Column types are inferred from the
 * event field types feeding each column (a literal like 'todo' → text, a
 * counter → integer); a column fed by two events with conflicting types errors.
 * Pass `columns` to override inference for any column.
 */
export function defineProjection<S extends EventShapes>(
  name: string,
  catalog: EventCatalog<S>,
  build: (on: OnHelper<S>) => RuleMap<S>,
  opts: { table?: string; columns?: Record<string, ColType> } = {},
): ProjectionSpec {
  const on = new Proxy({}, { get: () => ruleBuilder() }) as OnHelper<S>
  const rules = build(on)

  const columns: Record<string, ColType> = { ...(opts.columns ?? {}) }
  const wireOn: Record<string, OpRule> = {}

  for (const [evtType, raw] of Object.entries(rules) as Array<[string, RawRule]>) {
    if (raw.op === 'delete') {
      wireOn[evtType] = { op: 'delete' }
      continue
    }
    const set: SetMap = {}
    for (const [col, val] of Object.entries(raw.set ?? {})) {
      const path = pathOf(val)
      if (path) {
        set[col] = path
        if (!opts.columns?.[col]) inferColumn(columns, col, colTypeFromPath(catalog, evtType, path))
      } else {
        set[col] = val as string | number | boolean | null
        if (!opts.columns?.[col]) inferColumn(columns, col, colTypeFromLiteral(val))
      }
    }
    for (const col of Object.keys(raw.inc ?? {})) {
      if (!opts.columns?.[col]) inferColumn(columns, col, 'integer')
    }
    wireOn[evtType] = {
      op: 'upsert',
      ...(Object.keys(set).length ? { set } : {}),
      ...(raw.inc ? { inc: raw.inc } : {}),
    }
  }

  const def: ProjectionDef = { name, ...(opts.table ? { table: opts.table } : {}), columns, on: wireOn }
  return { def }
}

/** Resolve a "$.field" path to a column type via the event's zod schema. */
function colTypeFromPath<S extends EventShapes>(catalog: EventCatalog<S>, evtType: string, path: string): ColType {
  const schema = catalog.schemas[evtType]
  if (!schema) return 'text'
  const parts = path.slice(2).split('.')
  // Only top-level fields map to a scalar column; nested paths fold to JSON text.
  if (parts.length !== 1) return 'text'
  const shape = schema.shape as Record<string, z.ZodTypeAny>
  const field = shape[parts[0]!]
  return field ? colTypeOfZod(field) : 'text'
}

function colTypeFromLiteral(val: unknown): ColType {
  if (typeof val === 'number') return Number.isInteger(val) ? 'integer' : 'real'
  if (typeof val === 'boolean') return 'integer'
  return 'text'
}
