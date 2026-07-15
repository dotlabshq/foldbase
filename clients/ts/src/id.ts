/**
 * UUIDv7 generation for stream ids (aggregate identity) and, optionally,
 * client-supplied event ids. A stream id is ALWAYS client-generated — the
 * server can't mint it, since it is the aggregate's identity chosen at creation.
 *
 * No type prefix: the read model (`read_<name>`) already carries the type, so
 * the stream id is a bare, time-ordered UUIDv7 that becomes the read-model PK.
 */

function randomBytes16(): Uint8Array {
  const b = new Uint8Array(16)
  // Web Crypto is available in Node 18+ and every browser.
  crypto.getRandomValues(b)
  return b
}

/** A time-ordered UUIDv7 string (48-bit unix-ms prefix + random tail). */
export function uuidv7(): string {
  const b = randomBytes16()
  const ts = Date.now()
  b[0] = Math.floor(ts / 2 ** 40) & 0xff
  b[1] = Math.floor(ts / 2 ** 32) & 0xff
  b[2] = Math.floor(ts / 2 ** 24) & 0xff
  b[3] = Math.floor(ts / 2 ** 16) & 0xff
  b[4] = Math.floor(ts / 2 ** 8) & 0xff
  b[5] = ts & 0xff
  b[6] = (b[6]! & 0x0f) | 0x70 // version 7
  b[8] = (b[8]! & 0x3f) | 0x80 // variant 10
  const h = [...b].map((x) => x.toString(16).padStart(2, '0')).join('')
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

/** Mint a fresh stream id (aggregate identity). Alias of {@link uuidv7}. */
export const newStreamId = uuidv7
