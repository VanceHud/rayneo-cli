# RayNeo Token Broker (Scheme A)

Passive, read-only access-token mirror server for [`rayneo-cli`](../README.md).

Under **Scheme A**, your mobile RayNeo App remains the sole authority for login and token refresh. An iOS capture helper (such as the [Loon plugin](../loon/README.md)) mirrors only the short-lived `access_token` and safe device headers to this broker, and `rayneo-cli` pulls the latest token on demand (`rayneo sync`) without ever calling `/authapi/token/refresh` or kicking out your phone session.

Two drop-in implementations share the **exact same V1 HTTP contract**:

| Implementation | Directory | Storage | Best for |
| --- | --- | --- | --- |
| **Cloudflare Worker + SQLite Durable Object** | [`cloudflare/`](./cloudflare/) | Strongly consistent DO SQLite (Free & Paid plans) | Zero-ops HTTPS endpoint at the edge |
| **Self-hosted Go Server** | [`server/`](./server/) | Atomic `0600` local JSON file (zero external deps) | VPS, NAS, homelab, Docker, or systemd |

---

## Security Model

- **Zero `refresh_token` handling**: Neither broker accepts, stores, logs, or returns `refresh_token` / `refreshToken`. In the Cloudflare Worker, incoming payloads are validated and projected to the strict allowlisted DTO before crossing the Durable Object RPC boundary.
- **Strict 15-header & `base_url` allowlists**: `client_headers` is filtered to the exact 15 non-signed device/app headers from `internal/har/har.go` (`user-agent`, `platform`, `env`, `locale`, `physicalregion`, `time-zone`, `app-version`, `version`, `app-build-number`, `phone-os-version`, `phone-model`, `device-id`, `deviceid`, `device_id`, `device-code`). Headers like `xtcl-*`, `authorization`, `host`, `content-*`, `accept-encoding`, and `cookie` are always dropped. `base_url` is strictly validated against `^https://xr-api-[a-z0-9-]+\.rayneo\.(cn|com)$` on both broker and CLI client to prevent API redirect poisoning / SSRF.
- **Fail-closed Bearer authentication & optional read/write separation**: Every endpoint except `GET /health` requires Bearer auth verified via SHA-256 + constant-time comparison across all configured secret slots. Unset, short (`<32` chars), low-diversity, or placeholder secrets fail closed (`503 Service Unavailable`; Go binary refuses to start). Supports both legacy single `BROKER_SECRET` (full access) and least-privilege `BROKER_READ_SECRET` (`GET` only) + `BROKER_WRITE_SECRET` (`POST` / `DELETE` only).
- **At-rest protection & automatic expiry purge**: The Cloudflare Worker encrypts `access_token`, `account`, `device_id`, and `client_headers` in DO SQLite using **AES-256-GCM** (HKDF-SHA256 derived key + column-bound AAD, with optional dedicated `BROKER_ENCRYPTION_KEY`) and schedules a Durable Object Alarm to auto-purge state 24 hours after `expires_at` (the Go server likewise auto-purges expired state past the 24h grace window).
- **Edge, transport & observability hardening**: Responses set `Cache-Control: no-store`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`, `Vary: Authorization`, and HSTS on HTTPS. The Cloudflare Worker enforces HTTPS on non-loopback hosts (`426`), enables per-IP Workers Rate Limiting (`30 req / 60s`), rejects unexpected query parameters (`400`), and enables `observability.redact_query_string: true`. Raw `access_token` and broker secrets are never logged (only the 12-char SHA-256 `token_fp` fingerprint).
- **Bounded request bodies & anti-downgrade**: `POST /v1/token` enforces `Content-Type: application/json`, strict UTF-8, a 64 KiB body limit (`413`), anti-downgrade protection against stale out-of-order `loon:header` captures, and cross-account metadata reset when `account_id` changes.

---

## V1 HTTP API Contract

| Endpoint | Auth | Description |
| --- | --- | --- |
| `GET /health` | None | Liveness probe → `200 {"ok":true}` |
| `POST /v1/token` | Bearer (`BROKER_WRITE_SECRET` or `BROKER_SECRET`) | Upsert access token and merge optional metadata |
| `GET /v1/token` | Bearer (`BROKER_READ_SECRET` or `BROKER_SECRET`) | Fetch current token, TTL status, and ETag (`304` on matching `If-None-Match`, `404` if empty/purged) |
| `DELETE /v1/token` | Bearer (`BROKER_WRITE_SECRET` or `BROKER_SECRET`) | Clear stored token state from memory and durable storage |

Key behaviors:
- **TTL non-extension**: When `expires_in` is omitted or `0`:
  - If `access_token` is **new**, `expires_at = now + 7200s` with `expires_estimated: true`.
  - If `access_token` is **identical** to the stored token, existing `expires_at` and `expires_estimated` are preserved (TTL is never artificially extended).
  - When `expires_in > 0` (capped at `86400`), `expires_at = now + expires_in` with `expires_estimated: false`.
- **Metadata merging**: Omitting optional fields (`base_url`, `region`, `account`, `account_id`, `device_id`, `client_headers`) preserves existing non-empty values; `client_headers` merges keys rather than wiping previously synced headers.
- **ETag caching**: `GET /v1/token` returns `ETag: "rev-<revision>-<token_fp>"` and supports `If-None-Match` (including weak `W/` tags, comma-separated lists, and `*`) returning `304 Not Modified`.
- **Expired tokens still return `200`**: `GET /v1/token` returns `200` with `"expired": true, "remaining_seconds": 0` so clients can inspect the state and prompt the user to wake the mobile app.

---

## Option 1: Cloudflare Workers (SQLite Durable Object)

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/VanceHud/rayneo-cli/tree/main/broker/cloudflare)

Uses a SQLite-backed Durable Object (`TokenBrokerDO` declared via `exports` with `storage: "sqlite"` in [`cloudflare/wrangler.jsonc`](./cloudflare/wrangler.jsonc)), which provides strong consistency on **both Cloudflare Workers Free and Paid plans**.

### One-Click Deploy (Recommended)

1. Click the **Deploy to Cloudflare** button above — Cloudflare automatically clones [`broker/cloudflare/`](./cloudflare/), provisions the `TokenBrokerDO` SQLite Durable Object, and prompts you for **`BROKER_SECRET`** (declared via [`.dev.vars.example`](./cloudflare/.dev.vars.example) and [`package.json`](./cloudflare/package.json)).
2. Generate a strong random value (e.g. `openssl rand -hex 32`) and paste it into the `BROKER_SECRET` field on the deploy page.
3. *(Recommended in mainland China)* After deployment, bind a custom domain in **Workers & Pages → Settings → Domains & Routes** so iOS Loon and `rayneo-cli` can connect directly without proxying `*.workers.dev`.

### Manual CLI Deploy (Wrangler)

#### Prerequisites

- Node.js 22+
- A Cloudflare account

#### 1. Generate a strong `BROKER_SECRET`

```bash
openssl rand -hex 32
```

Save this value — you will configure it in Cloudflare Workers, your iOS Loon plugin, and `rayneo sync`.

#### 2. Install & deploy with Wrangler

```bash
cd broker/cloudflare
npm install

# Log in to Cloudflare (first time only)
npx wrangler login

# Deploy the Worker and provision the SQLite Durable Object migration
npm run deploy

# Store BROKER_SECRET securely as a Worker secret (never commit it to git)
npx wrangler secret put BROKER_SECRET
```

Wrangler prints your HTTPS URL (for example `https://rayneo-token-broker.<your-subdomain>.workers.dev`). You may also bind a custom domain in the Cloudflare Dashboard under **Workers & Pages → Settings → Domains & Routes**.

#### 3. Local development & unit tests

Unit tests run directly with Node 22's built-in test runner and `node:sqlite` (no `npm install` or Cloudflare credentials required):

```bash
cd broker/cloudflare
npm test
```

To run full local validation (typecheck + tests + dry-run bundle):

```bash
npm run check
```

To run locally with Wrangler:

```bash
# Copy example secret file (gitignored .dev.vars) and replace placeholder
cp .dev.vars.example .dev.vars
npm run dev
```

---

## Option 2: Self-Hosted Go Server

Zero external Go dependencies (`broker/server/` lives inside the repository's `github.com/VanceHud/rayneo-cli` module).

### Configuration

| Option | Env Variable | Default | Description |
| --- | --- | --- | --- |
| *(env only)* | `BROKER_SECRET` | *(required unless read/write set)* | Shared full-access Bearer secret (≥ 32 chars; backward compatible); server refuses to start if unset/weak |
| *(env only)* | `BROKER_READ_SECRET` | *(optional)* | Least-privilege read-only Bearer secret (`GET /v1/token` only; ≥ 32 chars) |
| *(env only)* | `BROKER_WRITE_SECRET` | *(optional)* | Least-privilege write Bearer secret (`POST` & `DELETE /v1/token` only; ≥ 32 chars) |
| `-listen` | `BROKER_LISTEN` | `:8787` | Listen address (bind `127.0.0.1:8787` behind a reverse proxy) |
| `-state` | `BROKER_STATE_FILE` | `~/.local/share/rayneo-broker/state.json` (or `$XDG_DATA_HOME/rayneo-broker/state.json`, fallback `./broker-state.json`) | Path to persistent state file (written atomically with `0600` perms; auto-purged 24h after expiry) |
| `-require-https` | `BROKER_REQUIRE_HTTPS` | `false` | When `true`/`1`, rejects requests without TLS or `X-Forwarded-Proto: https` (`426`) |

### 1. Build & run binary directly

From the repository root (`rayneo-cli/`):

```bash
go build -trimpath -ldflags="-s -w" -o bin/rayneo-broker ./broker/server

export BROKER_SECRET="$(openssl rand -hex 32)"
./bin/rayneo-broker -listen 127.0.0.1:8787
```

Run tests (including race detector):

```bash
go test -race -v ./broker/server
```

### 2. Run with Docker

Build from the repository root (`rayneo-cli/`):

```bash
docker build -f broker/server/Dockerfile -t rayneo-token-broker .

docker run -d \
  --name rayneo-token-broker \
  --restart unless-stopped \
  -p 127.0.0.1:8787:8787 \
  -e BROKER_SECRET="$(openssl rand -hex 32)" \
  -v rayneo-broker-data:/data \
  rayneo-token-broker
```

The container runs as non-root (`uid 65532`) on `gcr.io/distroless/static-debian12:nonroot` and persists state in `/data/state.json` (`0600`).

### 3. Run with systemd (Linux)

An example hardened unit file is provided in [`server/rayneo-broker.service`](./server/rayneo-broker.service):

```bash
# 1. Install binary
sudo go build -trimpath -ldflags="-s -w" -o /usr/local/bin/rayneo-broker ./broker/server

# 2. Create dedicated system user
sudo useradd --system --no-create-home --shell /usr/sbin/nologin rayneo-broker

# 3. Create restricted environment file holding BROKER_SECRET
sudo install -d -m 0750 -o root -g rayneo-broker /etc/rayneo-broker
printf 'BROKER_SECRET=%s\n' "$(openssl rand -hex 32)" \
  | sudo tee /etc/rayneo-broker/broker.env >/dev/null
sudo chown root:rayneo-broker /etc/rayneo-broker/broker.env
sudo chmod 0640 /etc/rayneo-broker/broker.env

# 4. Install and start unit
sudo cp broker/server/rayneo-broker.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now rayneo-broker
sudo systemctl status rayneo-broker
```

### 4. HTTPS Reverse Proxy (Required for public internet / iOS Loon)

Always expose the self-hosted Go server behind TLS (Caddy, Nginx, or Cloudflare Tunnel) rather than plain HTTP.

**Caddy (`Caddyfile`)**:

```caddy
broker.example.com {
    reverse_proxy 127.0.0.1:8787
}
```

**Nginx**:

```nginx
server {
    listen 443 ssl http2;
    server_name broker.example.com;

    ssl_certificate     /etc/letsencrypt/live/broker.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/broker.example.com/privkey.pem;

    client_max_body_size 64k;

    location / {
        proxy_pass http://127.0.0.1:8787;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
    }
}
```

---

## Smoke Testing

```bash
export BROKER_URL="https://broker.example.com"   # or http://127.0.0.1:8787
export BROKER_SECRET="your-secret"

# 1. Health check (no auth)
curl -fsS "$BROKER_URL/health"

# 2. Push a synthetic test token
curl -fsS -X POST "$BROKER_URL/v1/token" \
  -H "Authorization: Bearer $BROKER_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"access_token":"test-token-123","expires_in":7200,"source":"manual","base_url":"https://xr-api-p.rayneo.cn","region":"cn"}'

# 3. Fetch token and inspect ETag header
curl -i -sS "$BROKER_URL/v1/token" \
  -H "Authorization: Bearer $BROKER_SECRET"

# 4. Conditional fetch (replace ETag with value from step 3 -> expects 304 Not Modified)
curl -i -sS "$BROKER_URL/v1/token" \
  -H "Authorization: Bearer $BROKER_SECRET" \
  -H 'If-None-Match: "rev-1-..."'

# 5. Wipe test token
curl -fsS -X DELETE "$BROKER_URL/v1/token" \
  -H "Authorization: Bearer $BROKER_SECRET"
```
