# rayneo-cli

[中文](./README.md) | **English** · [Full CLI & Protocol Manual](./docs/CLI.md) · [Security Policy](./SECURITY.md) · [Contributing](./CONTRIBUTING.md)

[![Go Version](https://img.shields.io/badge/Go-1.21%2B-00ADD8?logo=go)](./go.mod)
[![CI](https://github.com/VanceHud/rayneo-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/VanceHud/rayneo-cli/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/VanceHud/rayneo-cli?include_prereleases)](https://github.com/VanceHud/rayneo-cli/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](./LICENSE)
[![Agent Skill](https://img.shields.io/badge/Agent_Skill-rayneo--cli-6E56CF)](./skills/rayneo-cli/SKILL.md)

`rayneo-cli` is a zero-dependency Go CLI and AI Agent Skill for querying, searching, and archiving personal **"全天智记" (Lifelog)** data from **RayNeo AI Glasses** — including monthly calendars, daily events, GPS `trailPoints`, AI Daily Stories, speaker-attributed sentence-level transcripts (conversations & standalone recordings), action tasks, AI suggestions, and AI chat history.

```text
$ rayneo events -date 2026-11-01 | jq -r '.data.events[] | "\(.startTimeFormatted)  \(.audioLengthFormatted)  \(.eventType)  \(.title)"'
09:30   42:15  audio  Morning Architecture Review
12:15   28:40  audio  Team Lunch & Roadmap Sync
15:00   55:10  audio  Client Performance Optimization Discussion
```

---

## Highlights

- **Pure Go Stdlib Single Binary**: Zero third-party runtime or Go module dependencies. Compiles into a single portable `rayneo` binary for macOS, Linux, and Windows (`amd64` / `arm64`).
- **Non-Intrusive `.har` Token Import (Recommended)**: Extracts your active `xtcl-access-token` and safe client headers directly from a `.har` packet capture while clearing `refreshToken` and enforcing `no_refresh=true` — **never kicking your mobile App session offline**.
- **Unified Dual-Store Transcripts**: A single `rayneo full` command seamlessly merges lifelog conversation transcripts (`/lifelog/api/v1/events/{id}/full`) and standalone audio recording tasks (`/clsapi/aiTask/getDataAllByTaskId`), exporting speaker labels, millisecond timestamps, and translations in `md`, `txt`, `srt`, or `json`.
- **Smart On-Disk Cache**: Historical days are cached locally for 30 days (`0600` permissions), making repeated queries and resumed bulk exports **0-network-request** operations. Pass `-refresh` anytime to bypass the local cache.
- **Gentle Bulk Archiving**: `rayneo export` enforces built-in `400ms` request spacing to archive date ranges into raw JSON, UTF-8 BOM CSV (Excel-ready), and structured Markdown, while `rayneo report` compiles exported daily stories into a single offline Markdown report.
- **Ready-to-Use AI Agent Skill**: Ships with [`skills/rayneo-cli/SKILL.md`](./skills/rayneo-cli/SKILL.md) and `rayneo help --json` machine-readable specifications for Claude Code, Codex, Antigravity, Cursor, and other coding agents.

---

## Installation

### 1. Via `go install` (Requires Go 1.21+)

```bash
go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest
rayneo -version
```

### 2. Prebuilt Binaries (GitHub Releases)

Download the archive for your OS (`darwin` / `linux` / `windows`) and architecture (`amd64` / `arm64`) from [GitHub Releases](https://github.com/VanceHud/rayneo-cli/releases), extract `rayneo`, and place it on your `$PATH`.

### 3. Build from Source

```bash
git clone https://github.com/VanceHud/rayneo-cli.git
cd rayneo-cli
make build          # Outputs ./bin/rayneo
./bin/rayneo -version

# Optional: install into $(go env GOPATH)/bin
make install
```

### 4. Install the AI Agent Skill

```bash
# Option A: Install via the skills CLI (recommended)
npx skills add VanceHud/rayneo-cli

# Option B: Use the bundled installer script (--target agents|claude|gemini|all)
./scripts/install-skill.sh --target all
```

---

## Authentication Guide (Why `.har` Mode Is Recommended)

RayNeo's backend enforces strict single-session token rotation:

1. Logging in via SMS (`POST /identify/quickLogin`) invalidates your phone App's `refreshToken`.
2. Calling token renewal (`POST /authapi/token/refresh`) **immediately invalidates** the currently active `access_token`, kicking your mobile App offline.

To use `rayneo` alongside your phone without disrupting mobile sync, **always prefer importing a token from a `.har` capture** (see [`skills/rayneo-cli/references/har-capture-guide.md`](./skills/rayneo-cli/references/har-capture-guide.md) for iOS/Android capture steps):

### Option 1: Import from `.har` Capture (Recommended — Keeps Phone Logged In)

Capture a brief session against `xr-api-p.rayneo.cn` using Loon, Reqable, Proxyman, or Charles and export `capture.har`:

```bash
# 1) Persistent import (clears refreshToken, sets no_refresh=true, saves to 0600 config.json)
rayneo har ./capture.har -verify

# Or: One-off ephemeral run (never touches ~/.config/rayneo-cli/config.json and never refreshes)
rayneo -har ./capture.har events -date 2026-11-01

# 2) Inspect active session & refresh protection status
rayneo whoami
```

> **Note**: Access tokens typically last ~2 hours (or rotate when the mobile App refreshes itself). Whenever you see `401 Authentication failed`, capture a fresh `.har` and run `rayneo har ./capture.har -verify`. Delete the `.har` file once imported.

### Option 2: Standalone SMS Verification Login (Fallback)

Use only if you do not mind re-authenticating on your phone later:

```bash
rayneo sendcode -account 13800000000 -area +86
rayneo login    -account 13800000000 -code 123456
```

---

## Common Workflows & Command Cheatsheet

### Common Workflows

```bash
# 1. Check which dates have lifelog entries and view one-line daily summaries
rayneo calendar -from 2026-10 -to 2026-11
rayneo count

# 2. Fetch a specific day's events + GPS trailPoints, then inspect one event's outline
rayneo events -date 2026-11-01
rayneo event  -id 100000000000000001

# 3. Read a specific date's AI Daily Story (Note: use `jd -date`; `journal` only returns the latest entry)
rayneo jd -date 2026-11-01

# 4. Export speaker-attributed transcripts (merges conversation + recording stores; md|txt|srt|json)
rayneo full -date 2026-11-01 -format md
rayneo full -id 100000000000000001 -format srt
rayneo full -date 2026-11-01 -out ./rayneo-export/transcripts

# 5. Query assistant action items, AI suggestions, and AI chat history
rayneo tasks
rayneo suggests
rayneo chat

# 6. Bulk-export a date range (400ms rate-limit spacing) and build an offline Markdown report
rayneo export -from 2026-11-01 -to 2026-11-07 -out ./rayneo-export -format all
rayneo report -dir ./rayneo-export -out ./rayneo-export/weekly-report.md -title "November Week 1 Lifelog"

# 7. Bypass the 5-minute local disk cache when querying newly synced glasses data from today
rayneo events -date 2026-11-01 -refresh
```

### Command Cheatsheet (All 24 Subcommands)

For complete flag tables, response schemas, cache policies, and troubleshooting, see **[`docs/CLI.md`](./docs/CLI.md)** (or run `rayneo help --json`).

| Category | Subcommands | Description |
|---|---|---|
| **Auth & Config** | `har` | **[Recommended]** Import `access_token` & client headers from `.har` (`no_refresh=true`) |
| | `whoami` / `config` / `logout` | Inspect session status / update base URL, region, or token / clear saved credentials |
| | `sendcode` / `login` / `refresh` | Fallback SMS login & manual token renewal (warning: affects other logged-in devices) |
| **Read-Only Queries** | `count` / `calendar` | Overall item counts / monthly lifelog availability & one-line daily summaries |
| | `events` / `event` | Daily events + GPS `trailPoints` / single-event summary, timestamped outline & sub-events |
| | `jd` / `journal` | AI Daily Story for a **specific date** (`jd -date`) / latest AI Daily Story (`journal`) |
| | `tasks` / `suggests` / `chat` | Assistant-generated action items / AI suggestions / paginated AI chat history |
| **Transcripts** | `full` | **Full speaker-attributed transcripts** (auto-falls back across conversation & recording stores) |
| | `recordings` / `transcript` | List recording & live-caption (`cc`) tasks / fetch `aiTask` recording clips directly |
| **Archive & Utility** | `export` / `report` / `cache` | Rate-limited bulk range export / offline Markdown report builder / inspect or clear disk cache |
| | `raw` / `help` | Send arbitrary signed gateway requests (for API exploration) / CLI help & `--json` spec |

---

## Protocol & Signing Overview (`TCL1-HMAC-SHA256`)

Every request to the RayNeo API gateway (`https://xr-api-p.rayneo.cn`) is authenticated with `TCL1-HMAC-SHA256`:

```text
canonicalHeaders = "xtcl-nonce:" + nonce + ";xtcl-timestamp:" + ts + ";"
canonicalRequest = METHOD + "\n" + PATH + "\n" + QUERY + "\n" + canonicalHeaders + "\n" + base64(sha256(body))
stringToSign     = "TCL1-HMAC-SHA256\n" + ts + "\n" + base64(sha256(canonicalRequest))
signature        = base64(hmac_sha256(appKey, stringToSign))

Authorization: TCL1-HMAC-SHA256 Credential=<appId>,SignedHeaders=xtcl-nonce;xtcl-timestamp;,Signature=<signature>
```

- Pass `-debug` to any command to print `canonicalRequest`, `stringToSign`, and `Authorization` to `stderr`.
- Optional environment variables `RAYNEO_APP_ID` and `RAYNEO_APP_KEY` allow overriding the built-in signing credentials without recompiling. See [`docs/CLI.md`](./docs/CLI.md) for full protocol details and edge cases.

---

## Disclaimer

- **Personal Data & Research Only**: This tool is intended solely for accessing and backing up **your own personal account data** and for interoperability research.
- **Strictly Read-Only**: All query, transcript, and export commands perform read-only requests. No cloud mutation, upload, or deletion endpoints are implemented.
- **Respect Rate Limits**: Keep request rates gentle; prefer `rayneo export` (built-in `400ms` spacing) and the default on-disk cache for multi-day pulls.
- **Credential Hygiene**: `.har` captures and `~/.config/rayneo-cli/config.json` contain live session tokens and device metadata — never commit them or paste them into public issues (see [`SECURITY.md`](./SECURITY.md)).
- **Unofficial Project**: Independently developed and **unaffiliated with, neither endorsed nor sponsored by RayNeo (雷鸟创新) or TCL**. Upstream API changes may break functionality at any time.

---

## License

Released under the [MIT License](./LICENSE) · Copyright (c) 2026 VanceHud
