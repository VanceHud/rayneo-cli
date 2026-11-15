# Capturing a RayNeo `.har` Token Safely (iOS & Android)

Using a `.har` (HTTP Archive) capture lets `rayneo` reuse your mobile App's active `xtcl-access-token` and client device headers **without** calling SMS login (`POST /identify/quickLogin`) or token renewal (`POST /authapi/token/refresh`), keeping your phone App logged in seamlessly.

---

## Essential Capture Requirements

Before starting any capture tool, verify these three rules:

1. **Target host**: `xr-api-p.rayneo.cn` (or wildcard `*.rayneo.cn`).
2. **HTTPS MITM decryption MUST be enabled** for `xr-api-p.rayneo.cn` with the tool's root CA certificate installed and trusted on the device. Without HTTPS decryption, the `.har` only records encrypted TLS `CONNECT` tunnels and will not contain the `xtcl-access-token` header.
3. **Export format**: Standard **HAR (`.har`)** JSON — not proprietary `.loon`, `.reqable`, or `.chls` session files.

---

## iOS Capture Steps

### Option A: Reqable (Free, Recommended)

1. Install **Reqable** on iOS (standalone or paired with Reqable Desktop) and complete its **Certificate Setup**:
   - Install the configuration profile in **Settings → General → VPN & Device Management**.
   - Enable full trust in **Settings → General → About → Certificate Trust Settings**.
2. Configure HTTPS decryption to include `*.rayneo.cn` (`xr-api-p.rayneo.cn`).
3. Tap **Start** in Reqable to begin recording.
4. Open the **RayNeo App** (雷鸟 AR / RayNeo AI), go to **全天智记 (Lifelog)**, and pull down to refresh (triggers `GET /lifelog/api/v1/count` or `GET /lifelog/api/v1/events` with HTTP `200`).
5. Return to Reqable and tap **Stop** immediately (1–5 requests are plenty).
6. Filter by `xr-api-p.rayneo.cn`, select the captured requests (or the session), choose **Export → HAR (`.har`)**, and AirDrop / transfer `capture.har` to your computer.

### Option B: Loon

1. In **Loon → MITM**, generate and install Loon's CA certificate, then enable full trust under **Settings → General → About → Certificate Trust Settings**.
2. Add `xr-api-p.rayneo.cn` to MITM **Hostname** and turn on **MITM**.
3. Enable **HTTP Catcher (抓包)** in Loon.
4. Open the **RayNeo App → 全天智记** and pull to refresh.
5. Stop **HTTP Catcher**, open the capture record, tap the share/export icon in the top-right corner, and export as **HAR (`.har`)**.

### Option C: Stream or Proxyman (iOS)

1. Install and trust the MITM CA certificate in iOS Settings (both Profile installation and Certificate Trust Settings).
2. Add `xr-api-p.rayneo.cn` to HTTPS sniffing / SSL Proxying domains.
3. Start capture → pull to refresh **全天智记** in the RayNeo App → stop capture → **Export HAR (`.har`)**.

---

## Android Capture Steps

### Option A: Desktop Wi-Fi Proxy (Reqable Desktop / Charles / Proxyman)

> Android 7+ apps generally ignore user-installed CA certificates unless routed or configured; using a Desktop proxy or Reqable's guided certificate flow is the most reliable non-root approach, or use a system-trusted CA on rooted devices.

1. Start **Reqable**, **Charles**, or **Proxyman** on your computer on the same Wi-Fi network as your phone.
2. Enable **SSL Proxying / HTTPS Decryption** for `xr-api-p.rayneo.cn:443` (`*.rayneo.cn`), and install/trust the proxy CA certificate on the device if supported by your ROM/App version.
3. Point your phone's Wi-Fi Manual Proxy to your computer's LAN IP and proxy port.
4. Open the **RayNeo App → 全天智记** and pull to refresh so at least one `https://xr-api-p.rayneo.cn/lifelog/...` request completes with HTTP `200`.
5. Remove the Wi-Fi proxy on your phone, then on desktop right-click the `xr-api-p.rayneo.cn` host/session and choose **Export Session → HTTP Archive (`.har`)**.

### Option B: On-Device Reqable / HttpCanary

1. Install **Reqable** or **HttpCanary** on Android and configure its VPN capture + CA certificate.
2. Filter target app to the **RayNeo App** (or domain `xr-api-p.rayneo.cn`).
3. Start capture → refresh **全天智记** in the RayNeo App → stop capture.
4. Export the session as **`.har`** and copy `capture.har` to your computer.

---

## Importing, Verifying & Cleaning Up

### 1. Preview & Import into `rayneo`

```bash
# Optional: inspect extracted metadata without writing config.json
rayneo har ./capture.har -dry-run

# Import token + safe device headers and verify online with a read-only count call
rayneo har ./capture.har -verify
```

What `rayneo har` does automatically:
- Scans all `.har` entries targeting `rayneo`, ignores `/authapi/token/refresh` request headers (which carry a `refreshToken` rather than an `access_token`), skips tokens that later received `401`, and selects the freshest valid `xtcl-access-token`.
- Saves credentials to `~/.config/rayneo-cli/config.json` with `0600` permissions, clears `refresh_token`, and sets `no_refresh: true` so `rayneo` never invalidates your phone session.

### 2. Security Hygiene (Delete `.har` Immediately)

A raw `.har` archive contains live authentication headers, device identifiers, and full API response bodies captured during recording.

1. **Delete the `.har` file right after import**:
   ```bash
   rm -f ./capture.har
   ```
   Also remove copies from `~/Downloads`, Trash, or chat/transfer history.
2. **Never commit or share `.har` files**: Never attach `.har` captures or `~/.config/rayneo-cli/config.json` to Git commits, pull requests, or GitHub Issues.
3. **When token expires (~2 hours)**: `xtcl-access-token` typically expires after about 2 hours (or when the phone App rotates its own token). When `rayneo` returns `401 Authentication failed`, repeat the 15-second capture above and run `rayneo har ./capture.har -verify`.
