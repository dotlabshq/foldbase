// End-to-end smoke: boot a real server, drive it with the TS client.
// Run: node --import tsx test/smoke.mjs   (Node 20+)
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'
import { bootServer, signJwt } from '../../../conformance/harness.mjs'
import { FoldBase, FoldBaseError, defineProjectionFromColumns as defineProjection, jsonCol } from '../src/index.ts'
import { z } from 'zod'

const here = dirname(fileURLToPath(import.meta.url))
const SECRET = 'client-smoke-secret-32-chars-minimum!'
const svc = () => signJwt({ sub: 'app', type: 'service' }, SECRET)

// server dir: prefer the Go binary if built, else the TS dist.
const goDir = resolve(here, '../../../go')
const tsDir = resolve(here, '../../..')
const useGo = process.env.SMOKE_TARGET !== 'ts'
const cmd = useGo ? './bin/foldbase' : 'node dist/index.js'
const dir = useGo ? goDir : tsDir

let failed = 0
const check = (name, cond, detail = '') => {
  console.log(`  ${cond ? '✓' : '✗'} ${name}${cond ? '' : `\n      ${detail}`}`)
  if (!cond) failed++
}

// A typed projection authored with zod (the authoring layer).
const notes = defineProjection('notes', z.object({
  owner: z.string(),
  text: z.string(),
  pinned: z.boolean(),
  tags: jsonCol(z.array(z.string())),
  created_at: z.number().int(),
}), {
  on: {
    NoteAdded: { op: 'upsert', set: { owner: '$.owner', text: '$.text', pinned: '$.pinned', tags: '$.tags', created_at: '$.createdAt' } },
    NoteDeleted: { op: 'delete' },
  },
})

console.log(`\n▶ TS client smoke against ${cmd} (${useGo ? 'Go' : 'TS'})\n`)
const server = await bootServer({ cmd, dir, env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET } })
try {
  const es = new FoldBase({ baseUrl: server.base, token: svc(), tenant: 'acme' })

  const health = await es.health()
  check('health ok', health.ok === true && health.service === 'foldbase')

  const reg = await es.putProjection(notes.def)
  check('putProjection', reg.ok === true && reg.name === 'notes')
  await es.putPolicy({ name: 'notes', role: '*', using: 'owner = :auth_uid' })

  const appended = await es.append('n1', 0, [{
    type: 'NoteAdded', streamId: 'n1', actor: 'u1',
    payload: { owner: 'u1', text: 'hello', pinned: true, tags: ['a', 'b'], createdAt: 111 },
  }])
  check('append projected', appended.projected === true && appended.version === 1)
  check('append stamps writtenBy', appended.events[0].metadata.writtenBy === 'app')

  // query as the end-user u1 (forwarded identity)
  const asU1 = es.withAuth({ uid: 'u1' })
  const q = await asU1.query('notes')
  check('query row count', q.rows.length === 1)

  // typed parse via the authoring layer: JSON + boolean coercion round-trips
  const row = notes.fromRow(q.rows[0])
  check('fromRow typed', row.owner === 'u1' && row.pinned === true && Array.isArray(row.tags) && row.tags[0] === 'a')

  // concurrency conflict surfaces as a typed 409
  let conflict = null
  try {
    await es.append('n1', 0, [{ type: 'NoteAdded', streamId: 'n1', actor: 'u1', payload: { owner: 'u1', text: 'dup', pinned: false, tags: [], createdAt: 1 } }])
  } catch (e) {
    conflict = e
  }
  check('409 FoldBaseError with actual', conflict instanceof FoldBaseError && conflict.status === 409 && conflict.actual === 1)

  // delete + rebuild
  await es.append('n1', 1, [{ type: 'NoteDeleted', streamId: 'n1', actor: 'u1', payload: {} }])
  const afterDel = await asU1.query('notes')
  check('delete removes row', afterDel.rows.length === 0)

  const rb = await es.rebuild()
  check('rebuild', rb.ok === true)
} finally {
  await server.stop()
}

console.log(`\n${failed === 0 ? '✅' : '❌'} TS client smoke ${failed === 0 ? 'passed' : `${failed} failed`}\n`)
process.exit(failed === 0 ? 0 : 1)
