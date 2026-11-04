#!/usr/bin/env bash
# Beendet die von run-all.sh gestarteten Prozesse und den Proxy-Container.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
pids="$here/logs/pids"
if [[ -f "$pids" ]]; then
  # Zeilen "pid name"; nur beenden, was wirklich unser Binary aus dev/bin ist (PID-Wiederverwendung).
  while read -r pid name; do
    [[ -n "$pid" && -n "${name:-}" ]] || continue
    if ps -p "$pid" -o command= 2>/dev/null | grep -qF "$here/bin/$name"; then
      kill "$pid" 2>/dev/null || true
    else
      echo "Überspringe PID $pid ($name): kein Prozess aus $here/bin/$name."
    fi
  done <"$pids"
  rm -f "$pids"
fi
podman rm -f orchivio-proxy >/dev/null 2>&1 || true
echo "Gestoppt. Postgres läuft weiter (podman compose -f dev/compose.yaml down zum Beenden)."
