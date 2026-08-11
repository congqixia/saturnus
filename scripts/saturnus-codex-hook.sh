#!/usr/bin/env bash
set -euo pipefail

root="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
agent="$root/bin/saturnus-agent"

if [[ ! -x "$agent" ]]; then
  echo "saturnus-agent binary not found at $agent" >&2
  echo "run ./scripts/install-codex-hook.sh first" >&2
  printf '{}\n'
  exit 0
fi

export SATURNUS_SERVER="${SATURNUS_SERVER:-http://localhost:8787}"
export SATURNUS_CODEX_APPROVAL_WAIT="${SATURNUS_CODEX_APPROVAL_WAIT:-9m}"

exec "$agent" codex-hook
