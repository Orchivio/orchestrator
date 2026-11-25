#!/usr/bin/env bash
# Startet den kompletten Orchivio-Stack aus den veröffentlichten Images (ghcr.io).
# ORCHIVIO_VERSION=<tag> wählt die Version (Standard: main).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$here/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$here/.env"
  set +a
fi

missing=()
for v in ORCHIVIO_SECRET_KEY ORCHIVIO_BOOTSTRAP_TOKEN LINEAR_CLIENT_ID LINEAR_CLIENT_SECRET_REF \
  GITHUB_APP_ID GITHUB_APP_PRIVATE_KEY GITHUB_APP_PRIVATE_KEY_REF GITHUB_CLIENT_ID GITHUB_CLIENT_SECRET_REF; do
  [[ -n "${!v:-}" ]] || missing+=("$v")
done
if [[ -z "${ANTHROPIC_API_KEY:-}" && -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]]; then
  missing+=("ANTHROPIC_API_KEY oder CLAUDE_CODE_OAUTH_TOKEN")
fi
if ((${#missing[@]} > 0)); then
  echo "Fehler: Folgende Variablen fehlen (dev/.env anlegen, siehe dev/env.example):" >&2
  printf '  - %s\n' "${missing[@]}" >&2
  exit 1
fi

# Podman-Socket in der VM; der Worker startet darüber die Sandboxen.
socket="$(podman info --format '{{.Host.RemoteSocket.Path}}')"
export ORCHIVIO_PODMAN_SOCKET="${socket#unix://}"
# Unter $HOME: podman machine bindet das Home-Verzeichnis unter gleichem Pfad ein.
export ORCHIVIO_WORK_DIR="${ORCHIVIO_WORK_DIR:-$HOME/.orchivio/work}"
mkdir -p "$ORCHIVIO_WORK_DIR"

compose=(podman compose -f "$here/compose.yaml")
if [[ "${ORCHIVIO_PULL:-always}" != never ]]; then
  "${compose[@]}" pull --quiet
fi
"${compose[@]}" up -d
"$here/seed.sh"
echo "Orchivio läuft (${ORCHIVIO_VERSION:-main}). Runs: GET http://localhost:8088/api/v1/tenants/<slug>/projects/<id>/runs (Bearer-Token)"
echo "Logs: podman compose -f dev/compose.yaml logs -f   Stoppen: dev/stop-all.sh"
