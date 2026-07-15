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
    // FB_DB_URL lets the whole run target a real database (e.g. Postgres);
    // otherwise each boot gets a fresh in-memory SQLite.
    env: { ...process.env, PORT: String(port), DB_URL: process.env.FB_DB_URL || ':memory:', ...env },
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

// ── SSE client (for realtime tests) ───────────────────────────────────────────
export async function openSSE(base, path, headers = {}) {
  const ctrl = new AbortController()
  const res = await fetch(`${base}${path}`, { headers, signal: ctrl.signal })
  const events = []
  const waiters = []
  const notify = () => {
    for (let i = waiters.length - 1; i >= 0; i--) {
      if (events.length >= waiters[i].n) waiters.splice(i, 1)[0].resolve()
    }
  }
  if (res.ok && res.body) {
    ;(async () => {
      try {
        const reader = res.body.getReader()
        const dec = new TextDecoder()
        let buf = ''
        for (;;) {
          const { value, done } = await reader.read()
          if (done) break
          buf += dec.decode(value, { stream: true })
          let idx
          while ((idx = buf.indexOf('\n\n')) >= 0) {
            const raw = buf.slice(0, idx)
            buf = buf.slice(idx + 2)
            if (raw.startsWith(':')) continue // heartbeat / comment
            const ev = {}
            for (const line of raw.split('\n')) {
              const c = line.indexOf(':')
              const field = line.slice(0, c)
              const val = line.slice(c + 1).replace(/^ /, '')
              if (field === 'id' || field === 'event' || field === 'data') ev[field] = val
            }
            if (ev.data !== undefined) {
              ev.json = JSON.parse(ev.data)
              events.push(ev)
              notify()
            }
          }
        }
      } catch {
        /* aborted */
      }
    })()
  }
  return {
    status: res.status,
    events,
    waitFor: (n, ms = 2500) =>
      new Promise((resolve, reject) => {
        if (events.length >= n) return resolve()
        const t = setTimeout(() => reject(new Error(`timeout: wanted ${n} SSE events, got ${events.length}`)), ms)
        waiters.push({ n, resolve: () => { clearTimeout(t); resolve() } })
      }),
    close: () => ctrl.abort(),
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
