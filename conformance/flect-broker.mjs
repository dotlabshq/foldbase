#!/usr/bin/env node
// A stand-in for the Flect broker's resolve endpoint, enough to run foldbase
// with DB_URL=flect://DB outside Flect (ADR-013).
//
//   GET /v1/bindings/:name/resolve  (Bearer $APP_TOKEN) → { manifest }
//
// MODE=direct   sqld url + namespace, as the broker's direct mode returns.
// MODE=gateway  data-gateway url + an HS256 data token in the format of
//               flect/packages/broker/src/gateway/token.ts; run Flect's own
//               gateway (src/gateway/server.ts) with the same KEY in front of sqld.
// TTL (seconds) sets the manifest's lifetime, so renewal can be watched.
import { createServer } from 'node:http'
import { createHmac } from 'node:crypto'
const { MODE = 'direct', PORT = '18094', TTL = '900', SQLD = 'http://127.0.0.1:18092',
  GATEWAY = 'http://127.0.0.1:7001', KEY = 'e2e-gateway-key', TENANT = 'acme', REF = 'clave-db-1', APP_TOKEN = 'app-token' } = process.env
const b64 = (s) => Buffer.from(s).toString('base64url')
const mint = (ttl) => {
  const body = `${b64(JSON.stringify({ alg: 'HS256', typ: 'JWT' }))}.${b64(JSON.stringify({ t: TENANT, d: REF, exp: Math.floor(Date.now() / 1000) + ttl }))}`
  return `${body}.${createHmac('sha256', KEY).update(body).digest('base64url')}`
}
let n = 0
createServer((req, res) => {
  const m = /^\/v1\/bindings\/([^/]+)\/resolve$/.exec(req.url)
  if (!m || req.method !== 'GET') { res.writeHead(404).end(); return }
  if (req.headers.authorization !== `Bearer ${APP_TOKEN}`) { res.writeHead(401).end('{"error":{"code":"UNAUTHORIZED"}}'); return }
  const ttl = Number(TTL)
  const connection = MODE === 'gateway' ? { url: GATEWAY, authToken: mint(ttl) } : { url: SQLD, namespace: `${TENANT}-${REF}` }
  console.log(`${new Date().toISOString()} resolve #${++n} ${m[1]} mode=${MODE}`)
  res.writeHead(200, { 'content-type': 'application/json' }).end(JSON.stringify({ manifest: {
    version: 1, binding: m[1], kind: 'database', provider: 'libsql', connection, capabilities: ['read', 'write'],
    expiresAt: new Date(Date.now() + ttl * 1000).toISOString() } }))
}).listen(Number(PORT), () => console.log(`fake broker :${PORT} mode=${MODE} ttl=${TTL}s`))
