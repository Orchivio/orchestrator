#!/usr/bin/env bash
# Beendet die von run-all.sh gestarteten Prozesse und den Proxy-Container.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/bin"
# shellcheck disable=SC1091
source "$here/pids.sh"
stop_recorded "$here/logs/pids"
podman rm -f orchivio-proxy >/dev/null 2>&1 || true
echo "Gestoppt. Postgres läuft weiter (podman compose -f dev/compose.yaml down zum Beenden)."
