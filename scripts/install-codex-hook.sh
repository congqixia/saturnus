#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mkdir -p "$root/bin"
make -C "$root" build-agent

cat <<MSG
Built:
  $root/bin/saturnus-agent

Codex hook config:
  $root/.codex/hooks.json

Before starting Codex, make sure Saturnus is running:
  make run-server

Optional environment:
  export SATURNUS_SERVER=http://localhost:8787
  export SATURNUS_CODEX_APPROVAL_WAIT=9m
  export SATURNUS_AUTO_PASS=1
MSG
