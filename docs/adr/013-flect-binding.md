# ADR-013: Resolve the database from a Flect binding (`DB_URL=flect://<binding>`)

- **Status**: accepted
- **Date**: 2026-10-27
- **Issues**: #12, which absorbs #11

## Context

On Flect, an app's container receives only `FLECT_TOKEN` and `FLECT_BROKER_URL`.
A database is a binding that the broker resolves into a manifest, and that
manifest's credentials expire:

```
GET {FLECT_BROKER_URL}/v1/bindings/DB/resolve   Authorization: Bearer FLECT_TOKEN
→ { manifest: { version: 1, kind: "database", provider: "libsql",
                connection: { url, authToken?, namespace? }, expiresAt } }
```

The broker has two modes:

| Mode | What the manifest holds |
|---|---|
| direct | sqld's own url and the namespace. The client stamps the namespace on each request as `x-namespace`. |
| gateway | the data gateway's url and an HS256 data token that lives 15 minutes. The gateway derives the namespace from the token and ignores any `x-namespace` the client sends. |

Two simpler approaches fail:

- **A fixed `DB_URL`.** It goes stale after 15 minutes in gateway mode, and Flect cannot refresh an env var.
- **A `DB_NAMESPACE` env (#11).** It would let whoever writes the config name any namespace. We had kept the header out of foldbase for exactly that reason.

Until this change, clave's foldbase ran on `file:` inside its container and lost everything on each restart.

## Decision

A fourth DB_URL form, `flect://<binding>`, is handled by `internal/flectdb`:

- **Resolve at boot.** An outage (network error, 5xx, 429) is retried with doubling backoff, five attempts by default. A refusal (any other 4xx) fails the boot at once with the broker's answer.
- **Accept only a manifest we can use.** It must be version 1, `kind: database`, `provider: libsql`, with a url and a future `expiresAt`. A namespace is accepted only over http(s) or libsql. The driver cannot send a header over a websocket, so the namespace would silently fall back to the default database.
- **Renew before expiry.** A `database/sql` connector picks the credentials for each new connection. Once less than a third of the manifest's life is left, the connector resolves again. The pool closes connections after a quarter of that life, so no connection outlives its token.
- **Survive a broker outage.** If renewal fails, the connector keeps the current credentials while they are valid and logs a warning. Only once they have expired do new connections fail. They recover as soon as the broker answers again.
- **Credentials come only from the broker.** A token becomes `Authorization`. A namespace becomes `x-namespace`, exactly as Flect's TS SDK does. There is no way to name a namespace by hand, so #11 is closed into #12. Keeping one tenant out of another's namespace is Flect's job: sqld auth plus the gateway. foldbase cannot enforce it from inside the container.

libsql is the only provider for now. A Postgres binding would go through the same connector, but Flect does not offer one to apps yet.

## Verification

- **Unit tests (`internal/flectdb`, fake broker and a Hrana stand-in).** They cover:
  - gateway mode sends the token and no namespace;
  - direct mode sends the broker's namespace;
  - renewal at the two-thirds mark;
  - the pool lifetime;
  - an outage keeps valid credentials and fails once they expire;
  - a refusal is not retried, while an outage is;
  - unusable manifests are refused.

  Breaking renewal or the namespace header turns the matching tests red.
- **`just persistence-flect`.** It runs the 23 persistence checks against a namespaced sqld through a stand-in broker (`conformance/flect-broker.mjs`) in direct mode. The data lands in the broker-named namespace and nowhere else.
- **Gateway mode, run by hand with Flect's own gateway** (`flect/packages/broker/src/gateway/server.ts`). The run passed 23/23. With 12-second tokens, 80 appends over 40 s all succeeded across four renewals, and the gateway recorded no expired token. sqld's pipeline responses carry no `base_url`, so the client never gets redirected around the gateway.

## Consequences

- A Flect app sets `DB_URL=flect://DB` (the binding name from its `flect.toml`) and drops the `file:` fallback. Its data survives restarts.
- foldbase now depends on version 1 of Flect's manifest contract. A different version fails the boot rather than being guessed at.
- **Known gap:** a token revoked before its `expiresAt` is noticed only at the next renewal. The failing query errors, the driver discards that connection, and the next one uses the same cached credentials until renewal. Forcing a refresh on 401 (`POST …/refresh`) is left for later.
