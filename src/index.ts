import { serve } from '@hono/node-server'
import { createClient, type Client } from '@libsql/client'
import { drizzle } from 'drizzle-orm/libsql'
import { createEventStoreRepo, events, SQLITE_SCHEMA_SQL } from '@baseworks/eventstore'
import { Registry, type SqlClient } from '@baseworks/readmodel'
import { buildApp } from './app.js'
import { assertAuthConfig, authMode } from './lib/auth.js'

import { homedir } from 'node:os'
import { join, dirname } from 'node:path'
import fs from 'node:fs'
import { createConfigManager } from '@baseworks/config'

/**
 * Database resolution, in order (first match wins):
 *
 *   1. Flect (FLECT_BROKER_URL+FLECT_TOKEN or FLECT_LOCAL_CONFIG) — resolve the
 *      binding via @getflect/sdk (default `DB`, override EVENTS_DB_BINDING).
 *      This is how a SIBLING foldbase deploys inside an app bundle: no
 *      substrate URL anywhere (ADR-0004). There is no shared central foldbase
 *      for apps — each app that wants one deploys its own.
 *   2. DB_URL (+ optional DB_NAMESPACE for multi-namespace sqld) — direct infra
 *      env, used by the platform own foldbase.
 *   3. A local file DB (dev default).
 */
async function resolveClient(): Promise<{ client: Client; label: string }> {
  if (process.env['FLECT_BROKER_URL'] || process.env['FLECT_LOCAL_CONFIG']) {
    const { createEnv } = await import('@getflect/sdk')
    const binding = process.env['EVENTS_DB_BINDING'] ?? 'DB'
    const client = await createEnv().db<Client>(binding)
    return { client, label: `flect binding '${binding}'` }
  }

  const defaults = { db: 'file:' + join(homedir(), '.cio', 'cio.db') }
  const cfg = createConfigManager<{ db: string }>('cio', defaults, { format: 'toml', layered: true }).load()
  const dbUrl = process.env['DB_URL'] ?? cfg.db

  if (dbUrl.startsWith('file:')) {
    fs.mkdirSync(dirname(dbUrl.slice(5)), { recursive: true })
  }

  // Multi-namespace sqld: DB_NAMESPACE stamps x-namespace on every request so
  // the event log lives in its own persistent sqld namespace.
  const namespace = process.env['DB_NAMESPACE']
  const nsFetch: typeof fetch | undefined = namespace
    ? (input, init = {}) => {
        const headers = new Headers((init as RequestInit).headers)
        headers.set('x-namespace', namespace)
        return fetch(input, { ...(init as RequestInit), headers })
      }
    : undefined

  const client = createClient({ url: dbUrl, ...(nsFetch ? { fetch: nsFetch } : {}) })
  return { client, label: dbUrl + (namespace ? ` (ns=${namespace})` : '') }
}

// Fail closed on auth misconfiguration BEFORE opening a database (ADR-002).
assertAuthConfig()

const { client, label } = await resolveClient()

// Event-log schema + read-model registry (idempotent DDL on boot).
await client.executeMultiple(SQLITE_SCHEMA_SQL)
const db = drizzle(client, { schema: { events } })
const store = createEventStoreRepo(db, { events })
const registry = new Registry(client as unknown as SqlClient)
await registry.init()

const app = buildApp({ client: client as unknown as SqlClient, store, registry })

const port = Number(process.env['PORT'] ?? 3001)
serve({ fetch: app.fetch, port }, () => {
  console.log(`[foldbase] Listening on port ${port}`)
  console.log(`[foldbase] Auth mode: ${authMode()}`)
  console.log(`[foldbase] Database: ${label}`)
  console.log(`[foldbase] Projections: ${registry.listProjections().map((d) => d.name).join(', ') || '(none registered)'}`)
})
