import { z } from 'zod'
import type { ColType, OpRule, ProjectionDef } from './types.js'

/**
 * Zod-native projection authoring (inherited from @baseworks/readmodel). Declare
 * a read model with a Zod object schema — the single source of truth for the
 * row shape — and get the plain `def` to register plus typed serialize/parse
 * helpers. Structured columns are stored as JSON text via `jsonCol`.
 */

const JSON_COL = Symbol('foldbase.jsonCol')
type RowValue = string | number | boolean | null

export function jsonCol<T>(inner: z.ZodType<T>): z.ZodType<T> {
  const schema = z.preprocess(
    (v) => (typeof v === 'string' ? JSON.parse(v) : v),
    inner as z.ZodTypeAny,
  ) as z.ZodType<T>
  Object.defineProperty(schema, JSON_COL, { value: true, enumerable: false })
  return schema
}

function isJsonCol(s: unknown): boolean {
  return Boolean((s as Record<symbol, unknown>)?.[JSON_COL])
}

// Detect zod types by `_def.typeName` (stable across duplicate module instances).
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
function isBoolean(field: z.ZodTypeAny): boolean {
  return !isJsonCol(field) && typeName(unwrap(field)) === 'ZodBoolean'
}
function sqliteType(field: z.ZodTypeAny): ColType {
  if (isJsonCol(field)) return 'text'
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
      throw new Error(`foldbase: column type ${typeName(base) || 'unknown'} is not scalar — wrap in jsonCol()`)
  }
}

export interface ProjectionSpec<T extends Record<string, unknown>> {
  def: ProjectionDef
  row: z.ZodType<T & { id: string; updated_at: number }>
  toColumns(row: Partial<T>): Record<string, RowValue>
  fromRow(raw: Record<string, unknown>): T & { id: string; updated_at: number }
}

export function defineProjection<S extends z.ZodObject<z.ZodRawShape>>(
  name: string,
  columns: S,
  opts: { table?: string; on?: Record<string, OpRule> } = {},
): ProjectionSpec<z.infer<S>> {
  const shape = columns.shape as Record<string, z.ZodTypeAny>
  const cols: Record<string, ColType> = {}
  const jsonFields = new Set<string>()
  const boolFields = new Set<string>()

  for (const [key, field] of Object.entries(shape)) {
    cols[key] = sqliteType(field)
    if (isJsonCol(field)) jsonFields.add(key)
    else if (isBoolean(field)) boolFields.add(key)
  }

  const def: ProjectionDef = {
    name,
    ...(opts.table ? { table: opts.table } : {}),
    columns: cols,
    on: opts.on ?? {},
  }

  const row = columns.extend({ id: z.string(), updated_at: z.number() }) as unknown as z.ZodType<
    z.infer<S> & { id: string; updated_at: number }
  >

  return {
    def,
    row,
    toColumns(r) {
      const out: Record<string, RowValue> = {}
      for (const key of Object.keys(shape)) {
        const v = (r as Record<string, unknown>)[key]
        if (v === undefined) continue
        if (jsonFields.has(key)) out[key] = v === null ? null : JSON.stringify(v)
        else if (typeof v === 'boolean') out[key] = v ? 1 : 0
        else out[key] = v as RowValue
      }
      return out
    },
    fromRow(raw) {
      const pre: Record<string, unknown> = { ...raw }
      for (const key of boolFields) {
        if (key in pre && pre[key] !== null && pre[key] !== undefined) pre[key] = Boolean(pre[key])
      }
      return (row as z.ZodTypeAny).parse(pre) as z.infer<S> & { id: string; updated_at: number }
    },
  }
}
