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

# Optional zweiter Mandant beta (Jira + GitLab); nur wenn die Werte gesetzt sind.
seed_beta() {
  local jira_adapter="${ORCHIVIO_SEED_JIRA_ADAPTER:-jira-cloud}" token="${ORCHIVIO_SEED_JIRA_TOKEN:-}"
  local jira_key="${ORCHIVIO_SEED_JIRA_KEY:-}" gl_token="${ORCHIVIO_SEED_GITLAB_TOKEN:-}"
  local gl_path="${ORCHIVIO_SEED_GITLAB_PATH:-}" gl_repo="${ORCHIVIO_SEED_GITLAB_REPO:-}"
  local site="${ORCHIVIO_SEED_JIRA_SITE_URL:-}" jira_email="${ORCHIVIO_SEED_JIRA_EMAIL:-}"
  if [[ -z "$token$jira_key$gl_token$gl_path$gl_repo" ]]; then return 0; fi
  if [[ -z "$token" || -z "$jira_key" || -z "$gl_token" || -z "$gl_path" || -z "$gl_repo" ]]; then
    echo "Mandant beta übersprungen: ORCHIVIO_SEED_JIRA_{TOKEN,KEY} und ORCHIVIO_SEED_GITLAB_{PATH,TOKEN,REPO} müssen alle gesetzt sein"
    return 0
  fi
  case "$jira_adapter" in
    jira-cloud)
      if [[ -z "$site" || -z "$jira_email" ]]; then
        echo "Mandant beta übersprungen: jira-cloud braucht ORCHIVIO_SEED_JIRA_SITE_URL und ORCHIVIO_SEED_JIRA_EMAIL"
        return 0
      fi ;;
    jira-dc) ;;
    *)
      echo "Mandant beta übersprungen: ORCHIVIO_SEED_JIRA_ADAPTER muss jira-cloud oder jira-dc sein"
      return 0 ;;
  esac
  local slug=beta code
  code="$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" "$api/tenants/$slug")"
  if [[ "$code" == 404 ]]; then
    call POST /tenants "$(jq -n --arg slug "$slug" --arg tracker "$jira_adapter" \
      '{slug: $slug, displayName: $slug, trackerAdapter: $tracker, codeHostAdapter: "gitlab"}')" >/dev/null
    echo "Mandant $slug angelegt"
  elif [[ "$code" != 200 ]]; then
    echo "Fehler: GET /tenants/$slug lieferte HTTP $code" >&2
    return 1
  fi
  local profile connections
  profile="$(call GET "/tenants/$slug/profiles" | jq -r '[.[] | select(.name == "default")][0].id // empty')"
  if [[ -z "$profile" ]]; then
    profile="$(call POST "/tenants/$slug/profiles" "$(jq -n --arg model "$model" \
      '{name: "default", runtime: "claude-code", model: $model, maxDuration: "20m"}')" | jq -r .id)"
  fi
  connections="$(call GET "/tenants/$slug/connections")"
  # Formular-Verbindung; Tokens nur im Request-Body, nie in der Ausgabe.
  connect_form() { # connect_form KIND ADAPTER FIELDS_JSON
    if [[ "$(jq -r --arg k "$1" '[.[] | select(.kind == $k and .status == "active")] | length' <<<"$connections")" == 0 ]]; then
      call POST "/tenants/$slug/connections/$2/form" "$(jq -n --argjson f "$3" '{fields: $f}')" >/dev/null
      echo "Verbindung $2 angelegt"
    fi
  }
  local jira_fields
  jira_fields="$(jq -n --arg adapter "$jira_adapter" --arg token "$token" --arg site "$site" --arg email "$jira_email" \
    --arg jql "${ORCHIVIO_SEED_JIRA_JQL:-}" \
    '(if $adapter == "jira-cloud" then {siteUrl: $site, email: $email, token: $token} else {token: $token} end)
     + (if $jql == "" then {} else {jql: $jql} end)')"
  connect_form tracker "$jira_adapter" "$jira_fields"
  connect_form codehost gitlab "$(jq -n --arg path "$gl_path" --arg token "$gl_token" '{path: $path, token: $token}')"
  if [[ -z "$(call GET "/tenants/$slug/projects" | jq -r --arg key "$jira_key" '[.[] | select(.trackerKey == $key)][0].id // empty')" ]]; then
    call POST "/tenants/$slug/projects" "$(jq -n \
      --arg key "$jira_key" --arg profile "$profile" --arg repo "$gl_repo" --arg email "$email" \
      --arg gitlab_user "${ORCHIVIO_SEED_GITLAB_USER:-}" --arg jira_user "${ORCHIVIO_SEED_JIRA_USER:-}" '
      ([$email] + (if $jira_user == "" then [] else ["jira:" + $jira_user] end)
                + (if $gitlab_user == "" then [] else ["gitlab:" + $gitlab_user] end)) as $who | {
        name: "demo-jira", trackerKey: $key, profileId: $profile,
        repos: [{slug: $repo, baseBranch: "main", allowedPaths: [""]}],
        classifier: {taskLabels: ["orchivio:aufgabe"], questionLabels: ["orchivio:frage"], defaultRepo: $repo},
        roles: {questions: $who, tasks: $who},
        statusMap: {running: "In Progress", succeeded: "In Review", failed: "To Do"},
        reviewTrigger: "mention",
        maxDiffLines: 500}')" >/dev/null
    echo "Projekt demo-jira ($jira_key) angelegt"
  else
    echo "Projekt demo-jira ($jira_key) vorhanden"
  fi
}

# beta darf acme nie verhindern: eigene Subshell mit set -e, ein Fehler warnt nur.
set +e
(set -e; seed_beta)
beta_rc=$?
set -e
if ((beta_rc != 0)); then
  echo "Warnung: Mandant beta nicht vollständig angelegt (Exit $beta_rc); acme wird trotzdem angelegt" >&2
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
    url="$(call POST "/tenants/$tenant/connections/$adapter/authorize" '{}' | jq -er .url)"
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
