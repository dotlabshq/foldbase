import type { Context } from 'hono'
import { verifyHs256Jwt } from '@baseworks/auth/jwt'

/**
 * The service's trust boundary. Auth mode is DECLARED, not only inferred
 * (ADR-002): `FOLDBASE_AUTH` = none | service-jwt | user-jwt.
 *
 *   none        — no token; tenant from X-Tenant-ID; X-Auth-* trusted; control
 *                 plane open (or gated by FOLDBASE_ADMIN_TOKEN). Dev / internal.
 *   service-jwt — only service tokens (type:"service"). User tokens rejected.
 *   user-jwt    — service tokens (full) + user tokens (query-only data plane).
 *
 * Token → capabilities (ADR-003/004/005):
 *   service token: tenant via X-Tenant-ID (required, no default) unless the
 *                  token pins org_id (then header must match or be absent);
 *                  may append + use the control plane; forwards end-user
 *                  identity via X-Auth-* headers.
 *   user token:    tenant = org_id claim; identity from claims (headers ignored);
 *                  query-only — no append, no control plane.
 */

export type AuthMode = 'none' | 'service-jwt' | 'user-jwt'

function env(key: string, e: NodeJS.ProcessEnv = process.env): string | undefined {
  return e[`FOLDBASE_${key}`]
}

/** Read at call time (not module load) so tests can toggle the mode. */
function secret(): string | undefined {
  return env('JWT_SECRET') ?? process.env['JWT_SECRET']
}

export function authMode(): AuthMode {
  const m = env('AUTH')
  if (m === 'none' || m === 'service-jwt' || m === 'user-jwt') return m
  // Legacy inference (dev convenience only): a secret implies verified tokens.
  return secret() ? 'service-jwt' : 'none'
}

/** Boot-time guard — fail closed on misconfiguration (ADR-002). */
export function assertAuthConfig(e: NodeJS.ProcessEnv = process.env): void {
  const m = env('AUTH', e)
  if ((m === 'service-jwt' || m === 'user-jwt') && !(env('JWT_SECRET', e) ?? e['JWT_SECRET'])) {
    throw new Error(`FOLDBASE_AUTH=${m} requires FOLDBASE_JWT_SECRET`)
  }
  if (e['NODE_ENV'] === 'production' && !m) {
    throw new Error('production requires an explicit FOLDBASE_AUTH (set "none" to run open)')
  }
}

export interface AuthCtx {
  uid?: string
  role?: string
  email?: string
}

export interface Resolved {
  tenant: string
  ctx: AuthCtx
  /** May append events (data-plane write). */
  canWrite: boolean
  /** May register definitions / run admin (control plane). */
  canControl: boolean
  /** The verified token subject, if any — stamped into event metadata for audit. */
  subject?: string
}

export type AuthResult = Resolved | { error: string; status: 400 | 401 | 403 }

function headerCtx(c: Context): AuthCtx {
  return {
    uid: c.req.header('X-Auth-UID'),
    role: c.req.header('X-Auth-Role'),
    email: c.req.header('X-Auth-Email'),
  }
}

function adminOk(c: Context): boolean {
  const admin = env('ADMIN_TOKEN')
  if (!admin) return true
  const token = c.req.header('Authorization')?.replace(/^Bearer\s+/i, '')
  return token === admin
}

export function resolveAuth(c: Context): AuthResult {
  const mode = authMode()

  if (mode === 'none') {
    const tenant = c.req.header('X-Tenant-ID')
    if (!tenant) return { error: 'X-Tenant-ID header is required', status: 400 }
    return { tenant, ctx: headerCtx(c), canWrite: true, canControl: adminOk(c) }
  }

  const s = secret()
  if (!s) return { error: 'server misconfigured: auth enabled without secret', status: 401 }

  const token = c.req.header('Authorization')?.replace(/^Bearer\s+/i, '')
  if (!token) return { error: 'missing bearer token', status: 401 }
  const claims = verifyHs256Jwt(token, s)
  if (!claims) return { error: 'invalid or expired token', status: 401 }

  const isService = claims['type'] === 'service'

  if (!isService) {
    if (mode === 'service-jwt') return { error: 'user tokens are not accepted in service-jwt mode', status: 401 }
    const tenant = claims['org_id']
    if (typeof tenant !== 'string' || tenant.length === 0) return { error: 'invalid or unscoped token', status: 401 }
    const sub = typeof claims['sub'] === 'string' ? claims['sub'] : undefined
    return {
      tenant,
      ctx: {
        uid: sub,
        role: typeof claims['role'] === 'string' ? claims['role'] : undefined,
        email: typeof claims['email'] === 'string' ? claims['email'] : undefined,
      },
      canWrite: false,
      canControl: false,
      subject: sub,
    }
  }

  // service token — tenant selection (ADR-004)
  const pin = typeof claims['org_id'] === 'string' && claims['org_id'].length > 0 ? claims['org_id'] : undefined
  const header = c.req.header('X-Tenant-ID')
  let tenant: string
  if (pin) {
    if (header && header !== pin) return { error: 'X-Tenant-ID conflicts with the token org_id', status: 403 }
    tenant = pin
  } else {
    if (!header) return { error: 'X-Tenant-ID header is required for service tokens', status: 400 }
    tenant = header
  }
  return {
    tenant,
    ctx: headerCtx(c),
    canWrite: true,
    canControl: true,
    subject: typeof claims['sub'] === 'string' ? claims['sub'] : undefined,
  }
}
