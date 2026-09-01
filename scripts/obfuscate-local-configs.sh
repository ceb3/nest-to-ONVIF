#!/usr/bin/env bash
# Obfuscate local config.yaml and tokens.json (backs up to *.bak), then write
# scripts/setup-screenshot-draft.yaml for docs screenshots.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if [[ ! -f config.yaml ]]; then
  echo "config.yaml not found in $ROOT" >&2
  exit 1
fi

ARGS=(-config config.yaml -in-place -backup -draft scripts/setup-screenshot-draft.yaml)
if [[ -f tokens.json ]]; then
  ARGS+=(-tokens tokens.json)
fi

go run ./scripts/obfuscate_config.go "${ARGS[@]}"
echo "Done. Originals saved as config.yaml.bak and tokens.json.bak (if present)."
echo "Re-auth with ./bin/nest-bridge auth before running serve again."
