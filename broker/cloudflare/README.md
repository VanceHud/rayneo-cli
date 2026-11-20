# RayNeo Token Broker (Cloudflare Worker)

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/VanceHud/rayneo-cli/tree/main/broker/cloudflare)

Passive, read-only `access_token` mirror for [`rayneo-cli`](https://github.com/VanceHud/rayneo-cli) backed by a strongly consistent **SQLite Durable Object** (`TokenBrokerDO`, supported on both Cloudflare Workers Free and Paid plans).

- **Zero `refreshToken` persistence**: `refresh_token` / `refreshToken` is stripped and never stored, logged, or returned.
- **Fail-closed Bearer auth**: All `/v1/token` endpoints require `Authorization: Bearer <BROKER_SECRET>` (constant-time SHA-256 compare).
- **ETag caching & safe metadata merge**: Supports `If-None-Match` (`304 Not Modified`), preserves existing device headers, and never artificially extends token TTL.

## One-Click Deploy

1. Click **Deploy to Cloudflare** above.
2. When prompted for **`BROKER_SECRET`**, paste a strong random secret (for example generated via `openssl rand -hex 32`).
3. Wait for Cloudflare Workers Builds to provision `TokenBrokerDO` and finish deployment.
4. *(Recommended in mainland China)* Attach a custom domain under **Workers & Pages → Settings → Domains & Routes** so iOS Loon and `rayneo-cli` can connect directly without proxying `*.workers.dev`.

## Connect iOS Loon & `rayneo-cli`

1. Import the Loon plugin on iOS:
   ```text
   https://raw.githubusercontent.com/VanceHud/rayneo-cli/main/loon/rayneo-token-sync.plugin
   ```
   Fill in `broker_url` (your Worker URL) and `broker_secret` (the exact `BROKER_SECRET` you set above), then refresh the RayNeo App once.
2. Bind `rayneo-cli`:
   ```bash
   rayneo sync -url https://<your-worker-host> -secret <your-broker-secret> -verify
   ```

## Local Development & Tests

```bash
npm install
npm test            # Node 22 built-in test runner + node:sqlite (no Cloudflare auth needed)
npm run typecheck   # tsc --noEmit
cp .dev.vars.example .dev.vars
npm run dev         # local Wrangler dev server
```
