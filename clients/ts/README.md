# @baseworks/foldbase

TypeScript client for [foldbase](https://github.com/dotlabshq/foldbase) — append
events, query read models, and author typed projections over HTTP. Wire-compatible
with any implementation of `openapi.yaml` (the TS reference **or** the Go server).

`zod` only. No drizzle, no libsql — the client core is `fetch` + types; `zod`
powers the optional authoring layer.

## Install

```bash
pnpm add @baseworks/foldbase
```

## Quick start

```ts
import { FoldBase } from '@baseworks/foldbase'

const fb = new FoldBase({
  baseUrl: process.env.FOLDBASE_URL!,   // e.g. http://localhost:8080
  token: process.env.FOLDBASE_TOKEN,    // service or user JWT (omit in `none`-mode dev)
  tenant: 'acme',                       // X-Tenant-ID (service tokens / none mode)
})

// append with optimistic concurrency (throws FoldBaseError(409) on conflict)
await fb.append('note-1', 0, [
  { type: 'NoteAdded', streamId: 'note-1', actor: 'u1', payload: { text: 'hi' } },
])

// query a registered read model
const { rows } = await fb.query('notes', { where: { owner: { eq: 'u1' } } })
```

## Client API

Constructed with `ClientOptions`: `{ baseUrl, token?, tenant?, auth?, fetch? }`.
`withAuth(auth)` and `withTenant(tenant)` return re-scoped copies (immutable);
`fetch` is injectable for tests.

**Streams (data plane)**

| Method | Purpose |
|---|---|
| `append(streamId, expectedVersion, events, { streamType? })` | append with OCC → `AppendResult` |
| `streamVersion(streamId)` | current version |
| `readStream(streamId, { fromVersion?, limit? })` | events of one stream |
| `readAll({ fromGlobalSeq?, limit?, type? })` | tenant log in global order; `type` narrows category |
| `readByCorrelation(correlationId, { limit? })` | events sharing a correlation id |
| `query<Row>(name, request)` | query a read model → `QueryResult<Row>` |

**Definitions + admin (control plane — needs a service token)**

| Method | Purpose |
|---|---|
| `putProjection(def)` | register/replace a read model |
| `putPolicy(def)` | set a row policy |
| `rebuild(name?)` / `reload()` | rebuild projections / reload defs |
| `health()` | `/healthz` |

**Realtime**

```ts
const sub = fb.subscribe({ type: 'task' }, (e) => render(e))
// ordered, gap-free SSE; resumes from last globalSeq across reconnects
sub.close()
```

## Typed authoring

Event schemas drive projection columns (proxy-path capture). `emit` type-checks
the payload at compile time and validates it (zod) before append:

```ts
import { defineAggregate, defineEvents } from '@baseworks/foldbase'
import { z } from 'zod'

const events = defineEvents({
  NoteAdded: z.object({ owner: z.string(), text: z.string() }),
  NotePinned: z.object({}),
})

const fb2 = fb.catalog(events)
await fb2.emit('note-1', 0, 'NoteAdded', { owner: 'u1', text: 'hi' }) // typed + validated
```

Projections are written against the same catalog. A field read off `e` becomes
a `$.` payload path, and its zod type decides the column type:

```ts
import { defineProjection } from '@baseworks/foldbase'

const notes = defineProjection('notes', events, (on) => ({
  NoteAdded: on.NoteAdded.upsert((e) => ({ owner: e.owner, text: e.text, pinned: false })),
  NotePinned: on.NotePinned.upsert(() => ({ pinned: true })),   // boolean column
}))
await fb.putProjection(notes.def)
```

Each rule is `upsert(set, { inc?, key? })`, `inc(counters, { key? })` or
`delete({ key? })`:

- **`inc`** adds to a column on every matching event. A number is a literal
  (`{ moves: 1 }` counts events); a field is a payload path, so the fold keeps
  the total (`(e) => ({ on_hand: e.quantity })`). A missing or non-numeric
  value fails the fold (`projected:false`) instead of counting as zero.
- **`key`** picks the row. Omitted, it is the stream the event landed on. Given
  a field, the row comes from the payload, so one row can total events from many
  streams. Every rule in a projection has a key, or none does.
- **`boolean`** columns come back as JSON `true`/`false`; an unset one is `null`.

```ts
const stock = defineProjection('stock', inventory, (on) => ({
  StockMoved: on.StockMoved.inc((e) => ({ on_hand: e.quantity }), { key: (e) => e.sku }),
  SkuDiscontinued: on.SkuDiscontinued.delete({ key: (e) => e.sku }),
}))
```

Those three need the `ghcr.io/dotlabshq/foldbase` image 0.4.0 or later. An
older server silently ignores `key` and keeps one row per stream (see
CHANGELOG.md). Lower-level column authoring is available via
`defineProjectionFromColumns` + `jsonCol`. Stream/event ids: `newStreamId()`,
`uuidv7()` (bare UUIDv7).

## Errors

Non-2xx responses throw `FoldBaseError(status, code, message?, actual?)` —
`status === 409` is an optimistic-concurrency conflict on `append`.

## Test

The smoke test is **end-to-end**: it boots a real foldbase server (the Go binary
if built, else the TS dist) and drives it with this client.

```bash
# Go target (default) — build the binary first: (cd ../../go && just build)
node --import tsx test/smoke.mjs
# TS reference target:
SMOKE_TARGET=ts node --import tsx test/smoke.mjs
```

`just typecheck`, `just build` and `just authoring` (the typed authoring
assertions) need no server; `just verify` runs all three before a publish.

## License

UNLICENSED — internal Baseworks package.
