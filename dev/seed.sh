#!/usr/bin/env bash
# Legt Mandant, Profil und Projekt über die Admin-API an (idempotent).
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

# Warten, bis die API antwortet.
curl --retry 30 --retry-connrefused --retry-delay 1 -fsS -o /dev/null \
  -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" "$api/tenants"

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
