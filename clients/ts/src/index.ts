export { FoldBase, TypedClient, type ClientOptions, type EmitOpts } from './client.js'
// Typed authoring layer (Approach B — event schemas drive columns; proxy paths).
export {
  defineEvents,
  defineAggregate,
  defineProjection,
  type EventCatalog,
  type EventShapes,
  type PayloadOf,
  type RuleBuilder,
  type ProjectionSpec,
} from './schema.js'
// Lower-level zod-column authoring + JSON columns (mode B / manual definitions).
export { defineProjection as defineProjectionFromColumns, jsonCol } from './projection.js'
// Stream/event id generation — bare UUIDv7 (aggregate identity = read-model PK).
export { newStreamId, uuidv7 } from './id.js'
export {
  FoldBaseError,
  type NewEvent,
  type StoredEvent,
  type AppendResult,
  type ColType,
  type OpRule,
  type ProjectionDef,
  type PolicyDef,
  type WhereOps,
  type WhereNode,
  type QueryRequest,
  type QueryResult,
} from './types.js'
