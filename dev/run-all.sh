#!/usr/bin/env bash
# Startet die komplette lokale Umgebung im Hintergrund.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
server_dir="$(dirname "$here")"
root="$(dirname "$server_dir")"
logs="$here/logs"
bin="$here/bin"
export GOWORK="$root/go.work"

if [[ -f "$here/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$here/.env"
  set +a
fi

missing=()
for v in LINEAR_TOKEN_ACME LINEAR_API_KEY_REF LINEAR_TEAM_KEYS GITHUB_APP_ID GITHUB_APP_PRIVATE_KEY GITHUB_APP_PRIVATE_KEY_REF DATABASE_URL; do
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

mkdir -p "$logs" "$bin"
# Nicht doppelt starten: laufende Prozesse aus einem früheren Start würden sonst verwaist.
# shellcheck disable=SC1091
source "$here/pids.sh"
if running="$(any_running "$logs/pids")"; then
  echo "Fehler: ${running% *} läuft noch (PID ${running#* }). Bitte zuerst dev/stop-all.sh ausführen." >&2
  exit 1
fi
: >"$logs/pids"

echo "Starte Postgres ..."
podman compose -f "$here/compose.yaml" up -d

echo "Starte Proxy und baue Images ..."
"$root/orchivio-worker/dev/up.sh"

echo "Baue Binaries ..."
(cd "$server_dir" && go build -o "$bin/orchivio-server" ./cmd/orchivio-server)
(cd "$root/orchivio-worker" && go build -o "$bin/orchivio-worker" ./cmd/orchivio-worker)
(cd "$root/orchivio-adapter-linear" && go build -o "$bin/orchivio-linear-adapter" ./cmd/orchivio-linear-adapter)
(cd "$root/orchivio-adapter-github" && go build -o "$bin/orchivio-github-adapter" ./cmd/orchivio-github-adapter)

# start <name> <env-Zuweisungen...> -- <befehl...>: Prozess im Hintergrund, Log und PID merken.
start() {
  local name="$1" binary="" arg
  shift
  for arg in "$@"; do [[ "$arg" == "$bin/"* ]] && { binary="${arg#"$bin"/}"; break; }; done
  nohup env "$@" >>"$logs/$name.log" 2>&1 &
  echo "$! $binary" >>"$logs/pids"
  echo "  $name (PID $!)"
}

echo "Starte Prozesse ..."
start linear-adapter ORCHIVIO_GRPC_ADDR=127.0.0.1:7101 ORCHIVIO_WEBHOOK_ADDR=:7102 "$bin/orchivio-linear-adapter"
start github-adapter ORCHIVIO_GRPC_ADDR=127.0.0.1:7201 "$bin/orchivio-github-adapter"
start server ORCHIVIO_CONFIG_DIR="$here/config" ORCHIVIO_GRPC_ADDR=127.0.0.1:7070 \
  ORCHIVIO_HTTP_ADDR=127.0.0.1:8088 ORCHIVIO_PUBLIC_URL=http://localhost:8088 "$bin/orchivio-server"
start worker ORCHIVIO_SERVER_ADDR=127.0.0.1:7070 ORCHIVIO_WORKER_ID=local-1 \
  ORCHIVIO_WORK_DIR="$here/work" ORCHIVIO_PROXY_URL=http://orchivio-proxy:8090 \
  ORCHIVIO_SANDBOX_IMAGE=localhost/orchivio-sandbox-claude:dev ORCHIVIO_SANDBOX_NETWORK=orchivio-sandbox \
  "$bin/orchivio-worker" run

echo "Warte auf den Server (max. 30 s) ..."
for _ in $(seq 1 30); do
  # Jede HTTP-Antwort (auch 404) zeigt, dass der Server lauscht.
  if curl -s -o /dev/null http://127.0.0.1:8088/; then
    echo "Server läuft. Logs: $logs/*.log, Timelines: http://localhost:8088/runs/<id>"
    echo "Stoppen mit dev/stop-all.sh"
    exit 0
  fi
  sleep 1
done
echo "Fehler: Server antwortet nach 30 s nicht. Siehe $logs/server.log (und die anderen Logs in $logs). Stoppe alles ..." >&2
"$here/stop-all.sh" >&2 || true
exit 1
