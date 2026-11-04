#!/usr/bin/env bash
# Beendet die von run-all.sh gestarteten Prozesse und den Proxy-Container.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pids="$here/logs/pids"
if [[ -f "$pids" ]]; then
  while read -r pid; do
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done <"$pids"
  rm -f "$pids"
fi
podman rm -f orchivio-proxy >/dev/null 2>&1 || true
echo "Gestoppt. Postgres läuft weiter (podman compose -f dev/compose.yaml down zum Beenden)."
