#!/usr/bin/env bash
# Install the rayneo-cli Agent Skill into local coding-agent skill directories.

set -euo pipefail

SKILL_NAME="rayneo-cli"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
SKILL_SRC="${REPO_ROOT}/skills/${SKILL_NAME}"

usage() {
  cat <<'EOF'
Usage: ./scripts/install-skill.sh [options]

Install the portable rayneo-cli Agent Skill into local AI agent skill directories.
(Tip: Node users can also install via `npx skills add VanceHud/rayneo-cli`.)

Options:
  -t, --target <name>   Target skill directory preset (default: agents)
                          agents  -> ~/.agents/skills/rayneo-cli   (Codex, Cursor, Antigravity, agentskills)
                          claude  -> ~/.claude/skills/rayneo-cli   (Claude Code)
                          gemini  -> ~/.gemini/config/skills/rayneo-cli (Gemini CLI / Antigravity config)
                          all     -> install to agents, claude, and gemini
      --dest <dir>      Install into custom skills parent directory (<dir>/rayneo-cli)
      --copy            Copy skill files into target (default; survives moving/deleting repo)
      --link            Symlink target to repo skill folder (useful while editing the skill)
  -n, --dry-run         Print planned operations without modifying any files
  -h, --help            Show this help message and exit

Examples:
  ./scripts/install-skill.sh
  ./scripts/install-skill.sh --target claude
  ./scripts/install-skill.sh --target all --copy
  ./scripts/install-skill.sh --target agents --link
EOF
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

TARGET="agents"
MODE="copy"
CUSTOM_DEST=""
DRY_RUN=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    -t|--target)
      [[ $# -ge 2 ]] || die "--target requires a value (agents|claude|gemini|all)"
      TARGET="$2"
      shift 2
      ;;
    --target=*)
      TARGET="${1#*=}"
      shift
      ;;
    --dest)
      [[ $# -ge 2 ]] || die "--dest requires a directory path"
      CUSTOM_DEST="$2"
      shift 2
      ;;
    --dest=*)
      CUSTOM_DEST="${1#*=}"
      shift
      ;;
    --copy)
      MODE="copy"
      shift
      ;;
    --link|--symlink)
      MODE="link"
      shift
      ;;
    -n|--dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown option: $1 (run with --help for usage)"
      ;;
  esac
done

[[ -d "${SKILL_SRC}" && -f "${SKILL_SRC}/SKILL.md" ]] || die "skill source not found at ${SKILL_SRC}/SKILL.md"

declare -a DEST_PARENTS=()

if [[ -n "${CUSTOM_DEST}" ]]; then
  DEST_PARENTS+=("${CUSTOM_DEST}")
else
  [[ -n "${HOME:-}" && "${HOME}" == /* ]] || die "HOME must be set to an absolute path"
  case "${TARGET}" in
    agents)
      DEST_PARENTS+=("${HOME}/.agents/skills")
      ;;
    claude)
      DEST_PARENTS+=("${HOME}/.claude/skills")
      ;;
    gemini)
      DEST_PARENTS+=("${HOME}/.gemini/config/skills")
      ;;
    all)
      DEST_PARENTS=(
        "${HOME}/.agents/skills"
        "${HOME}/.claude/skills"
        "${HOME}/.gemini/config/skills"
      )
      ;;
    *)
      die "invalid --target '${TARGET}' (expected: agents, claude, gemini, or all)"
      ;;
  esac
fi

validate_and_install() {
  local parent_raw="$1"
  [[ -n "${parent_raw}" ]] || die "empty destination parent path"

  # Normalize parent path without requiring it to exist yet.
  local parent_clean="${parent_raw%/}"
  [[ -n "${parent_clean}" ]] || die "refusing to install directly under filesystem root '/'"

  if [[ "${parent_clean}" != /* ]]; then
    parent_clean="$(pwd -P)/${parent_clean}"
  fi

  local dest="${parent_clean}/${SKILL_NAME}"

  # Safety guards against clobbering unrelated directories or the source itself.
  [[ "$(basename "${dest}")" == "${SKILL_NAME}" ]] || die "unexpected destination basename for ${dest}"
  [[ "${dest}" != "/" && "${dest}" != "${HOME:-}" && "${dest}" != "${REPO_ROOT}" ]] || die "unsafe destination path: ${dest}"

  if [[ -e "${parent_clean}" && ! -d "${parent_clean}" ]]; then
    die "parent path ${parent_clean} exists and is not a directory"
  fi

  if [[ -d "${dest}" && ! -L "${dest}" ]]; then
    local resolved_dest
    resolved_dest="$(cd "${dest}" && pwd -P)"
    if [[ "${resolved_dest}" == "${SKILL_SRC}" ]]; then
      die "destination ${dest} resolves to the skill source directory itself"
    fi
    # Only replace an existing directory if it is empty or a prior rayneo-cli skill install.
    if [[ -f "${dest}/SKILL.md" ]]; then
      if ! grep -Eq '^name:[[:space:]]*rayneo-cli([[:space:]]|$)' "${dest}/SKILL.md"; then
        die "refusing to overwrite ${dest}: existing SKILL.md does not belong to ${SKILL_NAME}"
      fi
    elif [[ -n "$(ls -A "${dest}" 2>/dev/null)" ]]; then
      die "refusing to overwrite non-empty unrelated directory: ${dest}"
    fi
  elif [[ -e "${dest}" && ! -L "${dest}" ]]; then
    die "refusing to overwrite existing non-directory file: ${dest}"
  fi

  if [[ "${DRY_RUN}" -eq 1 ]]; then
    printf '[dry-run] mkdir -p %s\n' "${parent_clean}"
    if [[ -L "${dest}" || -d "${dest}" ]]; then
      printf '[dry-run] replace existing %s\n' "${dest}"
    fi
    if [[ "${MODE}" == "link" ]]; then
      printf '[dry-run] ln -s %s %s\n' "${SKILL_SRC}" "${dest}"
    else
      printf '[dry-run] cp -R %s %s\n' "${SKILL_SRC}" "${dest}"
    fi
    return 0
  fi

  mkdir -p "${parent_clean}"

  if [[ -L "${dest}" ]]; then
    rm -f "${dest}"
  elif [[ -d "${dest}" ]]; then
    rm -rf "${dest}"
  fi

  if [[ "${MODE}" == "link" ]]; then
    ln -s "${SKILL_SRC}" "${dest}"
    printf 'linked  %s -> %s\n' "${dest}" "${SKILL_SRC}"
  else
    cp -R "${SKILL_SRC}" "${dest}"
    printf 'copied  %s -> %s\n' "${SKILL_SRC}" "${dest}"
  fi
}

for parent in "${DEST_PARENTS[@]}"; do
  validate_and_install "${parent}"
done

if [[ "${DRY_RUN}" -eq 0 ]]; then
  if command -v rayneo >/dev/null 2>&1; then
    printf 'cli     rayneo found at %s\n' "$(command -v rayneo)"
  else
    printf 'note    `rayneo` binary not yet on PATH; install with:\n'
    printf '        go install github.com/VanceHud/rayneo-cli/cmd/rayneo@latest\n'
  fi
fi
