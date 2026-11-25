#!/usr/bin/env bash
# Legt Mandant und Profil über die Admin-API an, das Projekt erst, wenn Tracker und
# Code-Host verbunden sind; sonst gibt es die Verbinden-Links aus (idempotent).
# Liest dev/.env; benötigt curl und jq. Gibt das Token nie aus.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ -f "$here/.env" ]]; then
  set -a
  # shellcheck disable=SC1091
  source "$here/.env"
  set +a
fi

: "${ORCHIVIO_BOOTSTRAP_TOKEN:?ORCHIVIO_BOOTSTRAP_TOKEN fehlt (dev/.env)}"
tenant="${ORCHIVIO_SEED_TENANT:-acme}"
team_key="${ORCHIVIO_SEED_TEAM_KEY:?ORCHIVIO_SEED_TEAM_KEY fehlt}"
repo="${ORCHIVIO_SEED_REPO:?ORCHIVIO_SEED_REPO fehlt}"
email="${ORCHIVIO_SEED_EMAIL:?ORCHIVIO_SEED_EMAIL fehlt}"
model="${ORCHIVIO_SEED_MODEL:-sonnet}"
api="${ORCHIVIO_API_URL:-http://localhost:8088/api/v1}"

call() { # call METHOD PATH [BODY]; Token nur im Header
  local method="$1" path="$2"
  shift 2
  local args=(-fsS -X "$method" -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN")
  [[ $# -gt 0 ]] && args+=(-H "Content-Type: application/json" -d "$1")
  curl "${args[@]}" "$api$path"
}

# Warten, bis die API antwortet; 401 heißt: das Bootstrap-Token passt nicht.
ready="$(curl --retry 30 --retry-connrefused --retry-delay 1 -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" "$api/tenants")"
if [[ "$ready" == 401 ]]; then
  cat >&2 <<'MSG'
Fehler: Die API lehnt das Token ab (HTTP 401). Das Bootstrap-Token gilt nur bei leerer Token-Tabelle.
Entweder ein Token anlegen und als ORCHIVIO_BOOTSTRAP_TOKEN in dev/.env setzen:
  podman compose -f dev/compose.yaml exec orchestrator /orchivio-server admin create-token
oder das Volume zurücksetzen (Datenverlust): podman compose -f dev/compose.yaml down -v
MSG
  exit 1
elif [[ "$ready" != 200 ]]; then
  echo "Fehler: GET /tenants lieferte HTTP $ready" >&2
  exit 1
fi

status="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" "$api/tenants/$tenant")"
if [[ "$status" == 404 ]]; then
  call POST /tenants "$(jq -n --arg slug "$tenant" \
    '{slug: $slug, displayName: $slug, trackerAdapter: "linear", codeHostAdapter: "github"}')" >/dev/null
  echo "Mandant $tenant angelegt"
elif [[ "$status" == 200 ]]; then
  echo "Mandant $tenant vorhanden"
else
  echo "Fehler: GET /tenants/$tenant lieferte HTTP $status" >&2
  exit 1
fi

profile_id="$(call GET "/tenants/$tenant/profiles" | jq -r '[.[] | select(.name == "default")][0].id // empty')"
if [[ -z "$profile_id" ]]; then
  profile_id="$(call POST "/tenants/$tenant/profiles" "$(jq -n --arg model "$model" \
    '{name: "default", runtime: "claude-code", model: $model, maxDuration: "20m", instructions: "Antworte auf Deutsch."}')" | jq -r .id)"
  echo "Profil default angelegt"
else
  echo "Profil default vorhanden"
fi

connections="$(call GET "/tenants/$tenant/connections")"
missing_links=()
for pair in tracker:linear codehost:github; do
  kind="${pair%%:*}" adapter="${pair##*:}"
  if [[ "$(jq -r --arg k "$kind" '[.[] | select(.kind == $k and .status == "active")] | length' <<<"$connections")" == 0 ]]; then
    url="$(call POST "/tenants/$tenant/connections/$adapter/authorize" '{}' | jq -r .url)"
    missing_links+=("$adapter: $url")
  fi
done
if ((${#missing_links[@]} > 0)); then
  echo "Verbindung fehlt; Link im Browser öffnen, danach seed.sh erneut ausführen:"
  printf '  %s\n' "${missing_links[@]}"
  echo "Projekt demo ($team_key) nicht angelegt"
  exit 0
fi

project_id="$(call GET "/tenants/$tenant/projects" | jq -r --arg key "$team_key" '[.[] | select(.trackerKey == $key)][0].id // empty')"
if [[ -z "$project_id" ]]; then
  call POST "/tenants/$tenant/projects" "$(jq -n \
    --arg key "$team_key" --arg profile "$profile_id" --arg repo "$repo" --arg email "$email" '{
      name: "demo", trackerKey: $key, profileId: $profile,
      repos: [{slug: $repo, baseBranch: "main", allowedPaths: [""]}],
      classifier: {taskLabels: ["orchivio:aufgabe"], questionLabels: ["orchivio:frage"],
                   repoLabels: {"repo:playground": $repo}, defaultRepo: $repo},
      roles: {questions: [$email], tasks: [$email]},
      maxDiffLines: 500}')" >/dev/null
  echo "Projekt demo ($team_key) angelegt"
else
  echo "Projekt demo ($team_key) vorhanden"
fi
