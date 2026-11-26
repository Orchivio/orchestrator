# Lokaler Ende-zu-Ende-Test

Diese Anleitung startet Orchivio komplett auf deinem Rechner (Server, Worker, Linear- und GitHub-Adapter, Postgres, Proxy) und prüft es gegen echtes Linear, echtes GitHub und echtes Claude. Alle Dienste laufen als Container aus den veröffentlichten Images unter `ghcr.io/orchivio/*`; lokal wird nichts gebaut.

Ohne externe Dienste läuft der automatische Rauchtest:

```sh
export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')
export TESTCONTAINERS_RYUK_DISABLED=true
go test -tags e2e ./internal/e2e/
```

## Voraussetzungen

- Podman mit `podman compose`, die Podman-Machine läuft (`podman machine start`)
- Go 1.26 nur für die Tests
- `gh` (GitHub CLI) optional, nützlich zum Prüfen der Pull Requests
- Ein Test-Repo, z. B. `soemii/orchivio-playground`, mit mindestens einem Commit auf `main`

## Vorbereitung

`dev/env.example` nach `dev/.env` kopieren (`cp dev/env.example dev/.env`). Die folgenden Abschnitte tragen ihre Werte dort ein. `dev/.env` ist gitignored; Secrets nie committen.

## Schlüssel erzeugen

Der Server verschlüsselt die Zugänge der Mandanten mit `ORCHIVIO_SECRET_KEY` und startet ohne ihn nicht. Einmalig erzeugen und in `dev/.env` eintragen; danach nicht mehr ändern, sonst sind gespeicherte Verbindungen unlesbar:

```sh
openssl rand -base64 32
```

## GitHub-App einrichten

1. GitHub, Settings, Developer settings, GitHub Apps, **New GitHub App**.
2. Name frei wählen, Homepage-URL beliebig. **Callback URL** und **Setup URL** (unter „Post installation“) beide `http://localhost:8088/oauth/callback/github`. Haken bei **Request user authorization (OAuth) during installation**. **Webhook** deaktivieren (kein Haken bei „Active“), es werden keine Webhooks benötigt.
3. Repository permissions:
   - Contents: Read and write
   - Pull requests: Read and write
   - Metadata: Read-only
4. Organization permissions: **Members: Read-only**. Damit prüft Orchivio, dass die verbindende Person Admin der Organisation ist. Für persönliche Konten ist nichts nötig. Nur Organisations-Admins bzw. der Kontoinhaber können eine Installation verbinden.
5. „Where can this GitHub App be installed?“: Only on this account (oder Any account für Organisationen).
6. App erstellen. Die **App ID** steht oben auf der Seite der App (Abschnitt „About“). Sie kommt als `GITHUB_APP_ID` in die `.env`; die **Client ID** als `GITHUB_CLIENT_ID`.
7. **Generate a new client secret** (Abschnitt „Client secrets“); der Wert kommt als `GITHUB_CLIENT_SECRET` in die `.env`.
8. Weiter unten **Generate a private key**. Die heruntergeladene `.pem`-Datei enthält den Schlüssel.
9. Den Inhalt der PEM-Datei in `dev/.env` als mehrzeiligen Wert in Anführungszeichen eintragen:

   ```
   GITHUB_APP_PRIVATE_KEY="-----BEGIN RSA PRIVATE KEY-----
   MIIE...
   -----END RSA PRIVATE KEY-----"
   ```

Die App wird nicht von Hand installiert: Das passiert beim Verbinden (siehe unten).

## Linear einrichten

Orchivio läuft in Linear als **App-User** einer OAuth-App: eigener Nutzer, erwähnbar und zuweisbar, kostet keinen Platz. Du brauchst Admin-Rechte im Workspace.

1. Settings, API, **OAuth applications**, neue App anlegen (<https://linear.app/settings/api/applications/new>). Name **Orchivio** (wird zum Anzeigenamen des App-Users). **Redirect URI**: `http://localhost:8088/oauth/callback/linear`. Webhooks aus. „Client credentials“ ist nicht mehr nötig.
2. Client-ID und Client-Secret kommen als `LINEAR_CLIENT_ID` und `LINEAR_CLIENT_SECRET` in die `.env`.
3. Den **Team-Key** (z. B. `ORC`) findest du in den Team-Einstellungen. Er gehört als `ORCHIVIO_SEED_TEAM_KEY` in die `.env` (und damit in `trackerKey` des Projekts).
4. Bedienung: In einem Ticket des Teams `@Orchivio` im Kommentar erwähnen, oder das Ticket an Orchivio delegieren (Zuweisen-Menü, Orchivio auswählen). Nach dem Verbinden erscheint Orchivio als App-User im Workspace.

## Mandant, Verbindungen und Projekt

`dev/run-all.sh` ruft nach dem Start `dev/seed.sh` auf. Das Skript legt über die Admin-API (`http://localhost:8088/api/v1`, Bearer-Token `ORCHIVIO_BOOTSTRAP_TOKEN`) Mandant und Profil `default` an. Das Projekt `demo` entsteht erst, wenn der Mandant eine aktive Tracker- und eine aktive Code-Host-Verbindung hat (sonst antwortet die API mit 422 „not connected“). Wiederholte Aufrufe ändern nichts. Die Werte kommen aus der `.env`:

- `ORCHIVIO_BOOTSTRAP_TOKEN`: Admin-Token, gilt nur bei leerer Token-Tabelle (Erzeugung siehe `dev/env.example`).
- `ORCHIVIO_SEED_TENANT`: Mandanten-Slug.
- `ORCHIVIO_SEED_TEAM_KEY`: dein Linear-Team-Key (`trackerKey`).
- `ORCHIVIO_SEED_REPO`: Test-Repo `owner/name` (Repo, `defaultRepo`, `repoLabels`).
- `ORCHIVIO_SEED_EMAIL`: deine Linear-E-Mail-Adresse für `roles.questions` und `roles.tasks`. Wer nicht gelistet ist, bekommt bei Erwähnung oder Zuweisung den Hinweis „Rolle fehlt“ und löst keinen Run aus; einfache Kommentare ohne Erwähnung werden ignoriert.
- `ORCHIVIO_SEED_MODEL`: Modell des Profils; ein Alias wie `sonnet` funktioniert mit Claude Code.

Rolleneinträge in `roles.questions`/`roles.tasks` sind E-Mail-Adressen, `<system>:<username>` oder `<system>:<id>` (`<system>` ist der `adapter_name`, z. B. `jira`, `gitlab`, `github`). Empfohlen ist `<system>:<id>` (Jira-`accountId` bzw. DC-Benutzerschlüssel, numerische GitLab-User-ID): IDs ändern sich nie, Benutzernamen und E-Mail-Adressen schon.

### Verbinden

Fehlt eine Verbindung, gibt `seed.sh` die Links aus (`POST /api/v1/tenants/acme/connections/{linear|github}/authorize`). Beide im Browser öffnen und zustimmen (GitHub: Installation auf das Test-Repo beschränken), danach `dev/seed.sh` erneut ausführen: Jetzt entsteht das Projekt. Den Stand zeigt `GET /api/v1/tenants/acme/connections` (`kind` tracker oder codehost, `status` active oder needs_reauth; bei `needs_reauth` den Link erneut erzeugen und öffnen).

### Mandanten-Admin-Token

Für den Alltag statt des Bootstrap-Tokens ein Token nur für einen Mandanten ausstellen (Antwort enthält das Token einmalig):

```sh
curl -fsS -X POST http://localhost:8088/api/v1/tokens \
  -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" -H "Content-Type: application/json" \
  -d '{"name": "acme-admin", "expiresIn": "720h", "tenant": "acme"}'
```

Ein weiteres Projekt lässt sich zur Laufzeit ohne Neustart anlegen (`<profile-id>` aus `GET /tenants/acme/profiles`; setzt aktive Verbindungen voraus):

```sh
curl -fsS -X POST http://localhost:8088/api/v1/tenants/acme/projects \
  -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" -H "Content-Type: application/json" \
  -d '{"name": "zweites", "trackerKey": "ABC", "profileId": "<profile-id>",
       "repos": [{"slug": "owner/name", "baseBranch": "main", "allowedPaths": [""]}],
       "classifier": {"taskLabels": ["orchivio:aufgabe"], "questionLabels": ["orchivio:frage"], "defaultRepo": "owner/name"},
       "roles": {"questions": ["you@example.com"], "tasks": ["you@example.com"]},
       "maxDiffLines": 500}'
```

Alle Ressourcen und Felder: `api/openapi.yaml`.

## Starten und Stoppen

```sh
dev/run-all.sh                         # zieht die Images (Tag main) und startet alles
ORCHIVIO_VERSION=1.2.0 dev/run-all.sh  # eine veröffentlichte Version
dev/stop-all.sh                        # stoppt alles, die Datenbank bleibt im Volume
```

`run-all.sh` meldet fehlende Variablen mit Namen (nie mit Werten). Nur der HTTP-Port (`127.0.0.1:8088`) ist vom Rechner aus erreichbar: Admin-API inkl. Runs (`/api/v1`, Bearer-Token über unverschlüsseltes HTTP, daher nur lokal); gRPC bleibt im Compose-Netz (unverschlüsselt, ohne Authentifizierung, mTLS vor entfernten Workern). Der Worker startet die Sandboxen über den Podman-Socket der Machine; sein Arbeitsverzeichnis liegt unter `~/.orchivio/work`, weil die Machine das Home-Verzeichnis unter gleichem Pfad einbindet. Mit `ORCHIVIO_PULL=never` nutzt `run-all.sh` lokal vorhandene Images.

## Live-Test Jira/GitLab

Die Adapter `adapter-jira` und `adapter-gitlab` liegen im Compose-Profil `jira-gitlab`; ohne Profil startet `dev/run-all.sh` genau dieselben Dienste wie bisher. Solange die Adapter-PRs nicht gemergt sind, gibt es die Images nicht auf GHCR: lokal bauen (`ghcr.io/orchivio/adapter-jira:main`, `ghcr.io/orchivio/adapter-gitlab:main` taggen) und mit `ORCHIVIO_PULL=never` starten.

Der Orchestrator kennt die Zusatz-Adapter nur über `ORCHIVIO_EXTRA_ADAPTERS`, das an `ORCHIVIO_ADAPTERS` angehängt wird (führendes Komma). Beides in `dev/.env` (siehe `env.example`) oder in der Umgebung setzen:

```sh
export COMPOSE_PROFILES=jira-gitlab
export ORCHIVIO_EXTRA_ADAPTERS=,jira-cloud=adapter-jira-cloud:7301,jira-dc=adapter-jira-dc:7302,gitlab=adapter-gitlab:7401
ORCHIVIO_PULL=never dev/run-all.sh
```

`JIRA_URL` (nur Data Center) und `GITLAB_URL` stehen in der `.env`. Container `jira-dc` und `gitlab-ce` sind bewusst nicht Teil des Stacks (große Images); es werden bestehende Instanzen genutzt.

`seed.sh` legt zusätzlich den Mandanten `beta` an (Tracker `jira-cloud`, oder `ORCHIVIO_SEED_JIRA_ADAPTER=jira-dc`; Code-Host `gitlab`), verbindet beide per Formular (`POST /api/v1/tenants/beta/connections/{adapter}/form`, Body `{"fields": {...}}`) und legt das Projekt `demo-jira` mit `statusMap` `{"running":"In Progress","succeeded":"In Review","failed":"To Do"}` an. Das passiert nur, wenn `ORCHIVIO_SEED_JIRA_{TOKEN,KEY}` und `ORCHIVIO_SEED_GITLAB_{PATH,TOKEN,REPO}` gesetzt sind (Jira Cloud zusätzlich `_SITE_URL`, `_EMAIL`). Tokens stehen nur in der `.env` und werden nie ausgegeben.

### Proxy und eigene CA

Die Adapter reichen `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` und `SSL_CERT_FILE` aus der `.env` durch. `SSL_CERT_FILE`/`SSL_CERT_DIR` zeigen auf ein CA-Bundle, das im Container liegen muss (Volume ergänzen, z. B. in einer `compose.override.yaml`). Der Worker braucht für `git` gegen GitLab hinter einer eigenen CA zusätzlich `GIT_SSL_CAINFO` (Pfad des Bundles im Worker bzw. in der Sandbox) sowie bei Bedarf dieselben Proxy-Variablen; sie sind im Compose nicht vorverdrahtet und kommen über eine Override-Datei.

### Checkliste (je Jira Data Center, Jira Cloud, GitLab CE/SaaS)

- [ ] Verbinden: `seed.sh` bzw. Formular-Endpoint; `GET /tenants/beta/connections` zeigt `active`.
- [ ] Projekt mit `statusMap` angelegt.
- [ ] Ticket dem Orchivio-Nutzer zuweisen: Run startet, Status wechselt auf „In Progress“.
- [ ] Run endet mit einem Merge Request; Ticket-Status wechselt auf „In Review“.
- [ ] Kommentar im MR mit Erwähnung von Orchivio: Antwort bzw. Folge-Run.
- [ ] Folge-Run auf demselben Branch: neuer Commit im bestehenden MR.
- [ ] Tokens widerrufen (Verbindung löschen): Die Projekt-Tokenliste in GitLab ist leer.
- [ ] Ergebnis (Datum, Instanz, Auffälligkeiten) hier eintragen.

## Bauen und Veröffentlichen

Jedes Repo baut in GitHub Actions (`.github/workflows/ci.yml`), trunk-based:

- Pull Request: Format, `go vet`, Tests (im Orchestrator auch der Ende-zu-Ende-Test).
- Push auf `main`: zusätzlich Images für amd64 und arm64 nach `ghcr.io/orchivio/<repo>` mit den Tags `main` und `sha-<commit>`.
- Tag `vX.Y.Z`: Images `X.Y.Z`, `X.Y` und `latest`.
- Contract: Tag `contract/vX.Y.Z` im Orchestrator-Repo veröffentlicht das Go-Modul `github.com/orchivio/orchestrator/contract`; Dependabot schlägt das Update in den anderen Repos vor. Lokal überschreibt `go.work` die Version, so dass Änderungen am Contract sofort überall greifen.

## Logs und Runs

- Logs: `podman compose -f dev/compose.yaml logs -f [orchestrator|worker|adapter-linear|adapter-github|proxy]`.
- Projekt-ID ermitteln: `curl -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" localhost:8088/api/v1/tenants/acme/projects` liefert die Projekte mit ihrer `id` (`<project-id>` unten).
- Runs ansehen (Bearer-Token nötig, neueste zuerst; Filter `ticket`, `state`, `limit`, `before`): `curl -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" localhost:8088/api/v1/tenants/acme/projects/<project-id>/runs`
- Einzelner Run mit Events: `curl -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" localhost:8088/api/v1/tenants/acme/projects/<project-id>/runs/<run-id>`. Die Run-ID steht in jedem Meilenstein-Kommentar im Ticket und im PR-Text.

## Manuelle Checkliste

- [ ] Linear-Ticket im Team `ORC`, Kommentar „@Orchivio Was steht in der README?“ ergibt eine Antwort im Ticket; die Run-ID im Kommentar lässt sich über die Runs-API abrufen.
- [ ] Ticket Orchivio zuweisen, Kommentar „Füge einen Abschnitt Kontakt in die README ein“ ergibt einen PR in `soemii/orchivio-playground` auf einem Branch `orchivio/orc-…`.
- [ ] Eine nicht konfigurierte Person erwähnt `@Orchivio` oder weist das Ticket Orchivio zu: Das ergibt den Hinweis „Rolle fehlt“, es startet kein Run. Ein einfacher Kommentar ohne Erwähnung von ihr wird still ignoriert (kein Hinweis).
- [ ] Unklare Aufgabe ergibt eine Rückfrage im Ticket; deine Antwort startet einen neuen Lauf mit Verlauf.

## Bekannte Grenzen des Durchstichs

- Keine OIDC-Absicherung der API (nur an `127.0.0.1` gebunden).
- gRPC ohne TLS und ohne Authentifizierung (Server, Adapter standardmäßig nur an `127.0.0.1` gebunden); mTLS vor entfernten Workern.
- Keine Kommentare im PR-Review.
- Kein Resume und keine Snapshots: Jede Antwort startet einen neuen Lauf.
- Nur Polling, keine Webhooks.
- Die Linear-GraphQL-Formen sind nicht gegen die Live-API verifiziert. Passiert nichts, zuerst die Logs von `adapter-linear` auf GraphQL-Fehler prüfen.
