# Security Policy / 安全策略

## Supported Versions

Only the latest release on the `main` branch is actively supported with security updates.

| Version | Supported |
|---|---|
| Latest (`main` / latest GitHub Release) | Yes |
| Older versions | No |

---

## Reporting a Vulnerability

If you discover a security vulnerability in `rayneo-cli` (for example: credential file permission regressions, unintended token leakage in logs, path traversal when writing exports/transcripts, or unsafe handling of untrusted `.har` / API inputs):

1. **Do NOT open a public GitHub Issue** containing exploit details or live credentials.
2. Please report it privately via **[GitHub Private Vulnerability Reporting](https://github.com/VanceHud/rayneo-cli/security/advisories/new)** on this repository.
3. Include minimal reproduction steps using **synthetic** inputs only.

---

## Credential & `.har` Safety Rules for Users

`rayneo-cli` interacts with your personal lifelog data. Please follow these practices to keep your account and personal archives safe:

### 1. Never Share `.har` Captures or Tokens in Issues / PRs

- A `.har` packet capture recorded from your phone contains **live bearer credentials** (`xtcl-access-token`, and potentially `refreshToken`), persistent device identifiers (`device-id`), account IDs, and raw lifelog/transcript responses.
- **NEVER** attach `.har` files, `~/.config/rayneo-cli/config.json`, or unredacted command outputs containing tokens to GitHub Issues, Discussions, Pull Requests, or public forums.
- If anyone obtains your active `xtcl-access-token`, they can read your lifelog data until the token expires. If you suspect a token was exposed, log out / re-authenticate in the RayNeo mobile App immediately to rotate server-side tokens, and run `rayneo logout` locally.

### 2. Delete `.har` Files After Importing

Once you import a session via:

```bash
rayneo har ./capture.har -verify
```

`rayneo` extracts only the token and minimal client headers into `~/.config/rayneo-cli/config.json`. **Delete the raw `./capture.har` file immediately afterward** (`rm ./capture.har`) so full HTTP capture payloads do not linger in your Downloads or workspace folders.

For zero-persistence runs where nothing is written to `config.json`, pass `-har ./capture.har` or `RAYNEO_TOKEN=...` for a single command and remove the capture when finished.

### 3. Local File Permissions (`0600`)

- **Configuration**: `~/.config/rayneo-cli/config.json` is created inside a `0700` directory and written with `0600` (`rw-------`) permissions so other local OS users cannot read your token.
- **Response Cache**: Cached responses under `~/.config/rayneo-cli/cache/` may contain personal lifelog events, GPS `trailPoints`, and transcripts; cache entries are likewise stored with `0600` permissions inside `0700` directories.
- **Cleanup**:
  - Run `rayneo logout` to wipe stored tokens, account metadata, and client headers from `config.json`.
  - Run `rayneo cache clear` (or pass `-no-cache` on sensitive shared machines) to remove cached API responses from disk.

### 4. Redacting `-debug` Output Before Filing Bug Reports

`rayneo` masks tokens in `whoami`, `har`, and `config` output. Note that `-debug` prints only the `TCL1-HMAC-SHA256` `Authorization` signature header (not `xtcl-access-token`), **however** raw JSON responses printed to `stdout` may contain personal event titles, GPS coordinates, or account IDs. Always scrub personal payloads, `accountId`, and `deviceId` values before sharing terminal snippets.
