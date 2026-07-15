// SSE subscribe smoke: boot Go, subscribe via the TS client, append, receive.
import { resolve, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { bootServer, signJwt } from '../../../conformance/harness.mjs'
import { Foldbase } from '../src/index.ts'

const here = dirname(fileURLToPath(import.meta.url))
const SECRET = 'ts-subscribe-secret-32-chars-minimum!'
const svc = signJwt({ sub: 'app', type: 'service' }, SECRET)

let failed = 0
const check = (n, c) => { console.log(`  ${c ? '✓' : '✗'} ${n}`); if (!c) failed++ }

const server = await bootServer({
  cmd: './bin/foldbase',
  dir: resolve(here, '../../../go'),
  env: { FOLDBASE_AUTH: 'service-jwt', FOLDBASE_JWT_SECRET: SECRET },
})
try {
  const fb = new Foldbase({ baseUrl: server.base, token: svc, tenant: 'acme' })
  const got = []
  const sub = fb.subscribe({ type: 'task' }, (e) => got.push(e))
  await new Promise((r) => setTimeout(r, 200)) // let the subscription establish

  await fb.append('t1', 0, [{ type: 'TaskCreated', streamId: 't1', actor: 'a', payload: { x: 1 } }], { streamType: 'task' })
  await fb.append('u1', 0, [{ type: 'UserAdded', streamId: 'u1', actor: 'a', payload: {} }], { streamType: 'user' }) // filtered out
  await fb.append('t1', 1, [{ type: 'TaskMoved', streamId: 't1', actor: 'a', payload: { x: 2 } }], { streamType: 'task' })

  // wait for the two 'task' events
  for (let i = 0; i < 30 && got.length < 2; i++) await new Promise((r) => setTimeout(r, 50))
  sub.close()

  check('received live task events', got.length === 2)
  check('in order (TaskCreated → TaskMoved)', got[0]?.type === 'TaskCreated' && got[1]?.type === 'TaskMoved')
  check('category filter excluded the user stream', got.every((e) => e.streamType === 'task'))
  check('events carry globalSeq + streamType', typeof got[0]?.globalSeq === 'number' && got[0]?.streamType === 'task')
} finally {
  await server.stop()
}
console.log(`\n${failed === 0 ? '✅' : '❌'} TS subscribe ${failed === 0 ? 'passed' : failed + ' failed'}\n`)
process.exit(failed === 0 ? 0 : 1)
