// Language-agnostic conformance harness for foldbase.
//
// It boots a server BINARY (any language) via a command, talks to it over real
// HTTP, and asserts the v1 contract (openapi.yaml + ADR 001-007). The server is
// the unit under test; the only coupling is the env contract below — every
// implementation MUST honor it:
//
//   PORT                     — listen port
//   DB_URL                   — libsql url; ":memory:" for an ephemeral run
//   FOLDBASE_AUTH          — none | service-jwt | user-jwt   (ADR-002)
//   FOLDBASE_JWT_SECRET    — HS256 realm secret (when auth != none)
//   FOLDBASE_ADMIN_TOKEN   — optional control-plane gate in `none` mode (ADR-003)
//
// Run:  node run.mjs --cmd "node dist/index.js" --dir /path/to/service
//
// Exit 0 iff every check in every suite passes.

import { spawn } from 'node:child_process'
import { createHmac } from 'node:crypto'
import net from 'node:net'

// ── JWT (HS256) — byte-identical to @baseworks/auth signHs256Jwt ──────────────
const b64u = (s) => Buffer.from(s).toString('base64url')

export function signJwt(claims, secret, ttl = 3600) {
  const now = Math.floor(Date.now() / 1000)
  const h = b64u(JSON.stringify({ alg: 'HS256', typ: 'JWT' }))
  const p = b64u(JSON.stringify({ iat: now, exp: now + ttl, ...claims }))
  const sig = createHmac('sha256', secret).update(`${h}.${p}`).digest('base64url')
  return `${h}.${p}.${sig}`
}

// ── free port ─────────────────────────────────────────────────────────────────
function freePort() {
  return new Promise((resolve, reject) => {
    const srv = net.createServer()
    srv.unref()
    srv.on('error', reject)
    srv.listen(0, '127.0.0.1', () => {
      const { port } = srv.address()
      srv.close(() => resolve(port))
    })
  })
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// ── server lifecycle ────────────────────────────────────────────────────────
export async function bootServer({ cmd, dir, env }) {
  const port = await freePort()
  const [bin, ...args] = cmd.split(' ')
  const child = spawn(bin, args, {
    cwd: dir,
    env: { ...process.env, PORT: String(port), DB_URL: ':memory:', ...env },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  let log = ''
  child.stdout.on('data', (d) => (log += d))
  child.stderr.on('data', (d) => (log += d))

  const base = `http://127.0.0.1:${port}`
  const deadline = Date.now() + 10_000
  while (Date.now() < deadline) {
    if (child.exitCode !== null) throw new Error(`server exited early (code ${child.exitCode}):\n${log}`)
    try {
      const r = await fetch(`${base}/healthz`)
      if (r.ok) return { base, stop: () => stop(child), log: () => log }
    } catch {
      /* not up yet */
    }
    await sleep(120)
  }
  stop(child)
  throw new Error(`server did not become healthy within 10s:\n${log}`)
}

function stop(child) {
  return new Promise((resolve) => {
    if (child.exitCode !== null) return resolve()
    child.on('exit', () => resolve())
    child.kill('SIGKILL')
    setTimeout(resolve, 1000)
  })
}

// ── HTTP client bound to a base url ───────────────────────────────────────────
export function client(base) {
  return async function call(method, path, { body, headers = {} } = {}) {
    const h = { ...headers }
    if (body !== undefined) h['Content-Type'] = 'application/json'
    const res = await fetch(`${base}${path}`, {
      method,
      headers: h,
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    })
    const text = await res.text()
    let json
    try {
      json = text ? JSON.parse(text) : undefined
    } catch {
      json = undefined
    }
    return { status: res.status, json, text }
  }
}

// ── assertion collector ───────────────────────────────────────────────────────
export function makeChecker(suiteName) {
  const results = []
  const ctx = {
    ok(name, cond, detail) {
      results.push({ suite: suiteName, name, pass: !!cond, detail: cond ? '' : detail ?? '' })
    },
    eq(name, actual, expected) {
      const pass = JSON.stringify(actual) === JSON.stringify(expected)
      results.push({
        suite: suiteName,
        name,
        pass,
        detail: pass ? '' : `expected ${JSON.stringify(expected)}, got ${JSON.stringify(actual)}`,
      })
    },
    status(name, res, expected) {
      const pass = res.status === expected
      results.push({
        suite: suiteName,
        name,
        pass,
        detail: pass ? '' : `expected HTTP ${expected}, got ${res.status} — ${res.text?.slice(0, 200)}`,
      })
    },
    results,
  }
  return ctx
}
