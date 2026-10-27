# Changelog — @baseworks/foldbase

## 0.2.0

Brings the client up to the fold rules the server gained after 0.1.1. Every
addition is optional, so 0.1.x code keeps compiling and behaving the same;
the one visible change is the boolean column type below.

The new rules need the server image `ghcr.io/dotlabshq/foldbase:0.4.0` or later.
On an older server (0.1.x) an `inc` path or a `boolean` column is rejected
when the projection is registered, but **`key` is silently ignored**: the
definition is accepted and rows stay keyed by stream, with no error. Upgrade
the server before registering a keyed projection.

- **Row `key`.** `upsert`, `inc` and `delete` take `{ key }` — a payload field
  (`(e) => e.sku`) or a `$.` path — so one row can total events from many
  streams. Omitted, the row is the stream, as before. `OpRule.key` on the wire
  type. (#6)
- **`inc` from the payload.** `inc` accepts a lambda over the event
  (`(e) => ({ on_hand: e.quantity })`) as well as literal numbers; the column
  type comes from the field's zod type, and a field that is not a number is
  rejected when the projection is defined. `OpRule.inc` values may be a `$.`
  path. (#5)
- **`boolean` column type.** `ColType` gains `'boolean'`, and `z.boolean()`
  fields and boolean literals now infer it instead of `'integer'`. The server
  returns such a column as JSON `true`/`false`. Storage is identical, so
  re-registering an existing projection needs no migration or rebuild. (#3)
- **`skipped` on rebuild results.** `putProjection` and `rebuild` resolve with
  `skipped`: the number of events the current rules could not fold.

## 0.1.1

Initial published client: append, reads, query, subscribe, typed authoring.
