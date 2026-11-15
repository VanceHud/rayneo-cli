# Contributing to `rayneo-cli`

Thank you for your interest in improving `rayneo-cli`! Because this tool handles personal lifelog data and authentication tokens, all contributions must follow strict engineering and privacy hygiene rules.

---

## Prerequisites

- **Go 1.21+** (pure standard library — please do not introduce external Go module dependencies without prior discussion)
- `make` and `bash` (for build targets and the Agent Skill installer script)

---

## Development Workflow

Clone the repository and run the standard verification suite before opening a Pull Request:

```bash
# 1. Build the CLI binary into ./bin/rayneo
make build
./bin/rayneo -version
./bin/rayneo help --json >/dev/null

# 2. Run static analysis
make vet

# 3. Run unit & race-detector tests
make test

# 4. Verify the Agent Skill installer script (if touching skills/ or scripts/)
bash -n scripts/install-skill.sh
./scripts/install-skill.sh --help

# 5. Clean local build artifacts before staging commits
make clean
```

### Repository Structure

- `cmd/rayneo/` — CLI entrypoint, flag parsing, subcommand routing, export & transcript formatters
- `internal/sign/` — `TCL1-HMAC-SHA256` canonical request & HMAC signing (`RAYNEO_APP_ID` / `RAYNEO_APP_KEY` overrides)
- `internal/har/` — `.har` packet capture parser and safe client-header extractor
- `internal/api/` — Signed HTTP client, compression handling, and cache integration
- `internal/cache/` — Local `0600` response cache and tiered TTL logic
- `internal/config/` — `0600` `config.json` persistence and `XDG_CONFIG_HOME` resolution
- `skills/rayneo-cli/` — Portable AI Agent Skill (`SKILL.md` and `references/har-capture-guide.md`)
- `scripts/install-skill.sh` — Cross-agent skill installer (`agents`, `claude`, `gemini`, `all`)
- `docs/CLI.md` — Full CLI and signing protocol reference manual

---

## Strict PII, Credential & Test-Vector Hygiene Rules

Every Pull Request and commit history entry is audited against leaks. **Never include any of the following in code, tests, fixtures, documentation, or commit messages:**

1. **Zero Real Credentials or Captures**:
   - Never commit `.har` packet captures, `.apk` / `.so` binaries, `config.json`, or exported lifelog directories.
   - Never include real `xtcl-access-token` or `refreshToken` bearer/JWT strings.
2. **Zero Personal Identifiers or Lifelog Text**:
   - Never include real phone numbers (always use obvious placeholders such as `13800000000`), real `accountId` values, real `device-id` / `imei` / `oaid` strings (use zeroed or synthetic UUIDs like `00000000-0000-4000-8000-000000000000`), GPS coordinates from real trips, or excerpts from real personal transcripts/journals.
3. **100% Synthetic Unit Test Vectors**:
   - All tests in `internal/sign/sign_test.go`, `internal/har/har_test.go`, and `cmd/rayneo/*_test.go` must construct **synthetic** tokens, synthetic timestamps, synthetic request parameters, and deterministically computed signatures.
4. **Zero Machine-Local Paths**:
   - Never hardcode absolute home-directory or workstation paths in source files, Markdown docs, or `skills/rayneo-cli/SKILL.md`. Use `rayneo` (assuming `$PATH`), `./bin/rayneo`, `./rayneo-export`, and `~` / `$HOME`.
5. **Check Your Git Author Email**:
   - Before committing, verify `git config user.email` so you do not accidentally embed a private or relay email address in public Git history (e.g., prefer `<username>@users.noreply.github.com`).

---

## Scope Guidelines

- **Read-Only Cloud Operations**: `rayneo-cli` intentionally restricts cloud operations to read-only lifelog/query endpoints (plus opt-in auth helpers). Pull Requests implementing destructive, mutating, or high-rate scraping endpoints will not be merged.
- **Multi-Device Safety First**: Any change touching authentication (`cmd/rayneo`, `internal/har`, `internal/config`) must preserve the invariant that `.har` / `no_refresh` sessions **never** invoke `/authapi/token/refresh` on `401` responses, and must keep `main_test.go` passing.
- **Documentation Sync**: When adding or modifying flags, environment variables, or subcommands, update `cmd/rayneo/help.go` (`rayneo help --json`), `docs/CLI.md`, `README.md`, `README_EN.md`, and `skills/rayneo-cli/SKILL.md` together.
