#!/usr/bin/env bash
# Stoppt den Orchivio-Stack. Die Datenbank bleibt im Volume erhalten
# (endgültig löschen: podman compose -f dev/compose.yaml down -v).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# COMPOSE_PROFILES='*': alle Profile, auch wenn mit anderen gestartet wurde.
COMPOSE_PROFILES='*' ORCHIVIO_PODMAN_SOCKET=/dev/null ORCHIVIO_WORK_DIR=/tmp podman compose -f "$here/compose.yaml" down
