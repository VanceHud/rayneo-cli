# RayNeo Token Broker (Cloudflare Worker)

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/VanceHud/rayneo-cli/tree/main/broker/cloudflare)

Passive, read-only `access_token` mirror for [`rayneo-cli`](https://github.com/VanceHud/rayneo-cli) backed by a strongly consistent **SQLite Durable Object** (`TokenBrokerDO`, supported on both Cloudflare Workers Free and Paid plans).

- **Zero `refreshToken` persistence**: `refresh_token` / `refreshToken` and non-allowlisted headers are stripped in the Worker isolate before crossing Durable Object RPC and are never stored, logged, or returned.
- **At-rest encryption & automatic expiry purge**: `access_token`, `account`, `device_id`, and `client_headers` are encrypted in DO SQLite with **AES-256-GCM** (HKDF-SHA256 derived key + column-bound AAD), and expired tokens are automatically purged via Durable Object Alarms 24 hours after `expires_at`.
- **Fail-closed Bearer auth with optional read/write separation**: Rejects unset, weak (`<32` chars), or placeholder secrets (`503`). Supports both single `BROKER_SECRET` (backward compatible) and least-privilege `BROKER_READ_SECRET` (`GET`) + `BROKER_WRITE_SECRET` (`POST`/`DELETE`).
- **Edge & transport hardening**: Enforces HTTPS (`426` on non-loopback HTTP) + HSTS, strict `base_url` allowlist (`https://xr-api-*.rayneo.(cn|com)`), per-IP Workers Rate Limiting (`30 req / 60s`), `observability.redact_query_string: true`, and `If-None-Match` (`304 Not Modified`) ETag caching.

## One-Click Deploy

1. Click **Deploy to Cloudflare** above.
2. When prompted for **`BROKER_SECRET`**, paste a strong random secret (≥ 32 chars, generated via `openssl rand -hex 32`).
3. Wait for Cloudflare Workers Builds to provision `TokenBrokerDO` and finish deployment.
4. *(Recommended, especially in mainland China)* Attach a custom domain under **Workers & Pages → Settings → Domains & Routes**, enable **Always Use HTTPS** in SSL/TLS, and disable `*.workers.dev` (set `"workers_dev": false` in `wrangler.jsonc` or disable the route in Dashboard) so traffic cannot bypass your Zone WAF/Access rules.

### Optional Least-Privilege Secret Separation

For multi-host or AI Agent setups, add separate secrets in **Workers & Pages → Settings → Variables and Secrets** (or via `wrangler secret put`):

- `BROKER_WRITE_SECRET` — configure in iOS Loon (`POST` / `DELETE /v1/token` only; `GET` returns `403`).
- `BROKER_READ_SECRET` — configure in `rayneo-cli` on desktop/server/agent hosts (`GET /v1/token` only; `POST`/`DELETE` return `403`).
- `BROKER_ENCRYPTION_KEY` — optional dedicated ≥ 32-char secret for DO SQLite AES-256-GCM encryption (defaults to HKDF derived from `BROKER_SECRET` / `BROKER_WRITE_SECRET` / `BROKER_READ_SECRET`; setting a dedicated key preserves stored state when rotating auth secrets).

Once both `BROKER_READ_SECRET` and `BROKER_WRITE_SECRET` are configured, you may remove `BROKER_SECRET` (or keep it temporarily during migration).

## Connect iOS Loon & `rayneo-cli`

1. Import the Loon plugin on iOS:
   ```text
   https://raw.githubusercontent.com/VanceHud/rayneo-cli/main/loon/rayneo-token-sync.plugin
   ```
   Fill in `broker_url` (`https://<your-worker-host>`) and `broker_secret` (`BROKER_WRITE_SECRET` or `BROKER_SECRET`), then refresh the RayNeo App once.
2. Bind `rayneo-cli` (prefer passing the secret via environment variable so it stays out of shell history and `ps` output):
   ```bash
   export RAYNEO_BROKER_URL="https://<your-worker-host>"
   export RAYNEO_BROKER_SECRET="<your-read-or-shared-secret>"
   rayneo sync -verify
   ```
   *(If you prefer persisting it into `~/.config/rayneo-cli/config.json` with `0600` permissions, run `rayneo sync -url "$RAYNEO_BROKER_URL" -secret "$RAYNEO_BROKER_SECRET" -verify` in a shell with history paused or unset afterward.)*

## Local Development & Tests

```bash
npm ci
npm test            # Node 22 built-in test runner + node:sqlite (no Cloudflare auth needed)
npm run typecheck   # tsc --noEmit
npm run check       # typecheck + unit tests + wrangler deploy --dry-run
cp .dev.vars.example .dev.vars
# Edit .dev.vars and set BROKER_SECRET=$(openssl rand -hex 32)
npm run dev         # local Wrangler dev server
```
