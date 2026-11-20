---
name: rayneo-cli
description: >-
  Query and export RayNeo AI glasses (雷鸟 AI 眼镜) "全天智记"
  (lifelog) data via the `rayneo` CLI: daily events, GPS trail points, AI daily
  journals (每日故事), full speaker-attributed transcripts (对话与录音逐句转录), assistant tasks,
  AI chats, and suggestions, or sync `xtcl-access-token` passively via a Scheme A
  Token Broker (`rayneo sync`) / `.har` packet captures without invalidating mobile
  App logins. Activate whenever the user mentions RayNeo, 雷鸟眼镜, 全天智记, lifelog,
  眼镜录音/转录, 每日故事, transcripts, `rayneo sync`, Token Broker, Loon sync, or `.har`
  token extraction for RayNeo.
---

# RayNeo AI Lifelog CLI (`rayneo`)

Operate the `rayneo` CLI (`github.com/VanceHud/rayneo-cli`) to query and export personal lifelog data from RayNeo AI glasses (`xr-api-p.rayneo.cn`) signed with `TCL1-HMAC-SHA256`.

## CLI & Environment Check

1. **Verify `rayneo` is on `$PATH`**:
   ```bash
   command -v rayneo && rayneo -version
   ```
   If `rayneo` is not installed yet:
   ```bash
   go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest
   # Or download a prebuilt binary from https://github.com/VanceHud/rayneo-cli/releases
   # Or inside a local repo clone (skills/rayneo-cli/SKILL.md): make build -> ./bin/rayneo
   ```
2. **Local state & cache paths** (override root via `$XDG_CONFIG_HOME`):
   - Credentials & client headers: `~/.config/rayneo-cli/config.json` (`0600`)
   - Local response cache: `~/.config/rayneo-cli/cache/` (`0600`)

---

## Authentication Rules (CRITICAL — Read First)

1. **Always prefer Scheme A Passive Token Broker (`rayneo sync`) or `.har` token extraction (`har` / `-har`) over SMS login or token refresh:**
   - **Why**: Calling `POST /identify/quickLogin` (`rayneo login`) invalidates the mobile App's `refreshToken`; calling `POST /authapi/token/refresh` (`rayneo refresh`) **immediately invalidates** the mobile App's active `access_token`, kicking the phone App offline.
   - **Option A — Scheme A Passive Token Broker (`rayneo sync`, best for ongoing automatic sync)**:
     - The mobile RayNeo App remains the sole token-refresh authority; an iOS Loon plugin passively mirrors only `access_token` + safe device headers (never `refreshToken`) to a private Token Broker.
     - First-time setup (saves `broker_url` & `broker_secret` into `0600` `config.json`, forces `no_refresh: true`, clears `refresh_token`, and optionally verifies via `GET /profileapi/v1/profile/query`):
       ```bash
       rayneo sync -url https://your-broker.workers.dev -secret <secret> -verify
       ```
     - Subsequent sync & inspection:
       ```bash
       rayneo sync            # conditional ETag pull (304 Not Modified when unchanged)
       rayneo sync -force     # skip If-None-Match and force full fetch
       rayneo sync -status    # show broker URL, masked secret, token fingerprint, and expiry
       rayneo sync -clear     # clear broker_url, broker_secret, broker_etag
       ```
     - Once configured (via `config.json`, `RAYNEO_BROKER_URL`/`RAYNEO_BROKER_SECRET`, or `-broker-url`/`-broker-secret`), query commands **auto-sync** before requests when token is missing/expired and **transparently retry once on 401** if the broker has a fresher token (bypassed when `-har` is active).
   - **Option B — Persistent `.har` import (zero-deployment alternative)**:
     ```bash
     rayneo har ./capture.har -verify
     ```
     Extracts the newest verified `xtcl-access-token` and safe device headers from Loon / Reqable / Proxyman / Charles `.har` files, clears `refresh_token`, and sets `no_refresh: true`.
   - **One-off ephemeral `.har` run (does not modify `config.json`, bypasses broker auto-sync)**:
     ```bash
     rayneo -har ./capture.har events -date YYYY-MM-DD
     ```
     (Also supported via `RAYNEO_HAR=./capture.har`.)
2. **Check current session before querying**:
   ```bash
   rayneo whoami
   rayneo sync -status   # if using a Token Broker
   ```
3. **Handling `401 Authentication failed` or `Missing token`**:
   - **NEVER** run `rayneo refresh`, `rayneo sendcode`, or `rayneo login` unless the user explicitly asks to authenticate via SMS (and understands it will affect their phone session).
   - **If a Token Broker is configured**: run `rayneo sync -force -verify`. If the broker still returns the same expired token (or `404 no token synced yet`), ask the user to **open/wake the RayNeo App on their phone (with Loon running)** so Loon mirrors a fresh `access_token`, then retry.
   - **If using `.har` mode**: ask the user for a fresh `.har` capture from their phone, import it with `rayneo har ./capture.har -verify`, and remind them to delete the `.har` afterward.
   - For step-by-step iOS & Android packet capture instructions, read [references/har-capture-guide.md](references/har-capture-guide.md).

---

## Core Read-Only Workflows

All query, transcript, export, report, and cache inspection commands are strictly **read-only**. Structured JSON prints to `stdout`; diagnostics print to `stderr` (exit codes: `0` success, `1` network/auth failure, `2` usage error).

### 1. Discover Available Dates & Overall Counts

```bash
# Calendar overview with per-day hasLog + one-line daily summaries
rayneo calendar -from 2026-10 -to 2026-11

# Overall task / schedule / activity counts
rayneo count
```

### 2. Inspect a Specific Day (Events, Outline, AI Daily Story)

```bash
# Day events + GPS trailPoints (JSON; optional -tz Asia/Shanghai)
rayneo events -date YYYY-MM-DD

# Single event digest (Markdown: summary, timestamped outline, sub-events, transcript sources; add -raw for JSON)
rayneo event -id <eventId>

# Specific day's AI Daily Story / 每日故事 (MUST use `jd -date`; `journal` ignores -date and only returns latest!)
rayneo jd -date YYYY-MM-DD
```

### 3. Pull Speaker-Attributed Transcripts (对话与录音逐句转录)

Transcripts live in two backend stores:
- `/lifelog/api/v1/events/{id}/full` — always-on lifelog conversations (`transcriptionContents[].clips[]`)
- `/clsapi/aiTask/getDataAllByTaskId` — standalone manual recordings (`recording.clips[]`)

**`rayneo full` queries both automatically** via fallback, so one command handles every event on a given day:

```bash
# Print a full day's or single event's transcript to stdout (formats: md | txt | srt | json)
rayneo full -date YYYY-MM-DD -format md
rayneo full -id <eventId> -format srt

# Write all transcripts of a day to disk
rayneo full -date YYYY-MM-DD -out ./rayneo-export/transcripts

# List standalone recording / live-caption tasks directly (-type recording | cc | '' for all)
rayneo recordings -type '' -n 50
rayneo transcript -id <taskId> -format md
```

### 4. Tasks, AI Chat History, Suggestions & Bulk Export

```bash
# Assistant action items, AI chat sessions, and AI suggestions
rayneo tasks
rayneo chat
rayneo suggests

# Bulk export a date range (JSON + UTF-8 BOM CSV + Markdown, with built-in 400ms rate-limit spacing)
rayneo export -from 2026-11-01 -to 2026-11-07 -out ./rayneo-export -format all

# Merge exported journal-*.md files into a single offline Markdown report (zero network calls)
rayneo report -dir ./rayneo-export -out ./rayneo-export/DAILY-REPORT.md

# Inspect or clear local on-disk cache (past days cached 30d, today 5m, counts/tasks/chat 10m)
rayneo cache stat
```

---

## Gotchas & Flags

- **Do not confuse `-refresh` (global cache flag) with `refresh` (auth subcommand)**:
  - `-refresh` (global flag, e.g. `rayneo -refresh events -date YYYY-MM-DD` or `rayneo events -date YYYY-MM-DD -refresh`): **safe** — bypasses local disk cache to fetch fresh data from the server. Use `-refresh` when querying **today's** newly synced glasses data within the 5-minute cache window.
  - `refresh` (subcommand `rayneo refresh`): **token renewal** — calls `POST /authapi/token/refresh`, invalidating the phone App's active access token (blocked automatically in HAR / `no_refresh` mode).
- **`jd -date` vs `journal`**: `rayneo journal` calls `GET /lifelog/api/v1/journal/latest`, which ignores `-date` and only returns the single most recent journal. Always use `rayneo jd -date YYYY-MM-DD` to fetch a specific date's 每日故事.
- **`cc` (Live Captions / 实时字幕) limitation**: The backend only stores task metadata for `cc` tasks, not sentence-level `clips`. Only lifelog conversation events (`full`) and `recording` tasks contain speaker-attributed `clips`.
- **Machine-readable CLI spec**: Run `rayneo help --json` to inspect all 25 commands, flags, HTTP paths, and `read_only` markers programmatically.
- **Environment overrides**: Supports `RAYNEO_BROKER_URL`, `RAYNEO_BROKER_SECRET`, `RAYNEO_HAR`, `RAYNEO_TOKEN`, `RAYNEO_NO_REFRESH=1`, `XDG_CONFIG_HOME`, and optional `RAYNEO_APP_ID` / `RAYNEO_APP_KEY` signing overrides.
