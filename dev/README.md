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

Optional steuert `commentTrigger` im Projekt (`any`, Standard, oder `mention`), ob in einem Ticket mit Konversation jeder Kommentar als Antwort oder Folgefrage zählt oder nur ein Kommentar mit Erwähnung des Bots.

Rolleneinträge in `roles.questions`/`roles.tasks` sind E-Mail-Adressen, `<system>:<username>` oder `<system>:id:<id>` (`<system>` ist der `adapter_name`, z. B. `jira`, `gitlab`, `github`; Beispiel `gitlab:id:12345`). Empfohlen ist `<system>:id:<id>` (Jira-`accountId` bzw. DC-Benutzerschlüssel, numerische GitLab-User-ID): IDs ändern sich nie, Benutzernamen und E-Mail-Adressen schon. IDs haben einen eigenen Namensraum, damit eine ID nie den gleichlautenden Benutzernamen eines anderen trifft; Benutzernamen, die mit `id:` beginnen, gelten nicht.

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

Die Adapter `adapter-jira` (als `adapter-jira-cloud` und `adapter-jira-dc`) und `adapter-gitlab` liegen im Compose-Profil `jira-gitlab`, Linear und GitHub im Profil `linear-github`. Ist `COMPOSE_PROFILES` nicht gesetzt, nimmt `dev/run-all.sh` `linear-github` und startet genau dieselben Dienste wie bisher; Linear-/GitHub-Variablen verlangt es nur, wenn `linear-github` aktiv ist. Die Images liegen auf GHCR, z. B. mit `ORCHIVIO_VERSION=0.1.0`.

Der Orchestrator kennt die Adapter über `ORCHIVIO_ADAPTERS` (Standard `linear=adapter-linear:7101,github=adapter-github:7201`), an das `ORCHIVIO_EXTRA_ADAPTERS` angehängt wird (führendes Komma). Er wählt Adapter erst bei Bedarf an, nicht laufende Adapter stören also nicht. Alles zusammen (in `dev/.env`, siehe `env.example`, oder in der Umgebung):

```sh
export COMPOSE_PROFILES=linear-github,jira-gitlab
export ORCHIVIO_EXTRA_ADAPTERS=,jira-cloud=adapter-jira-cloud:7301,jira-dc=adapter-jira-dc:7302,gitlab=adapter-gitlab:7401
dev/run-all.sh
```

Nur Jira und GitLab ohne Linear/GitHub: siehe [Arbeitslaptop](#arbeitslaptop-jira-data-center-und-gitlab). `dev/stop-all.sh` stoppt immer alle Profile.

`JIRA_URL` (nur Data Center) und `GITLAB_URL` stehen in der `.env`. Container `jira-dc` und `gitlab-ce` sind bewusst nicht Teil des Stacks (große Images); es werden bestehende Instanzen genutzt.

`seed.sh` legt zusätzlich den Mandanten `beta` an (Tracker `jira-cloud`, oder `ORCHIVIO_SEED_JIRA_ADAPTER=jira-dc`; Code-Host `gitlab`), verbindet beide per Formular (`POST /api/v1/tenants/beta/connections/{adapter}/form`, Body `{"fields": {...}}`) und legt das Projekt `demo-jira` mit `statusMap` `{"running":"In Progress","succeeded":"In Review","failed":"To Do"}` an. Das passiert nur, wenn `ORCHIVIO_SEED_JIRA_{TOKEN,KEY}` und `ORCHIVIO_SEED_GITLAB_{PATH,TOKEN,REPO}` gesetzt sind (bei `jira-cloud` zusätzlich `ORCHIVIO_SEED_JIRA_{SITE_URL,EMAIL}`; die Formularfelder richten sich nach dem Adapter, bei `jira-dc` nur `token`). Optional: `ORCHIVIO_SEED_JIRA_JQL` setzt den JQL-Filter der Verbindung; `ORCHIVIO_SEED_JIRA_USER` und `ORCHIVIO_SEED_GITLAB_USER` ergänzen die Rollen um `jira:<user>` bzw. `gitlab:<user>` (am besten die ID als `id:<id>`, z. B. `ORCHIVIO_SEED_GITLAB_USER=id:12345`, siehe oben). Rollen werden nur beim Anlegen des Projekts gesetzt. Schlägt beta fehl, gibt `seed.sh` eine Warnung aus und macht mit `acme` weiter. `acme` (Linear/GitHub) legt `seed.sh` nur an, wenn das Profil `linear-github` aktiv ist; sonst meldet es „Mandant acme übersprungen“. Tokens stehen nur in der `.env` und werden nie ausgegeben.

`adapter-jira-dc` startet ohne `JIRA_URL` nicht und startet im Profil `jira-gitlab` dann in einer Schleife neu. Wer nur Jira Cloud nutzt, stoppt den Container (`podman compose -f dev/compose.yaml stop adapter-jira-dc`); der Rest des Stacks läuft unabhängig davon.

### Proxy und eigene CA

- `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` gehen an die Jira-/GitLab-Adapter, an `proxy` und an `worker` (dort zusätzlich klein geschrieben für `git`; `orchestrator` und `orchivio-proxy` stehen im Worker immer in `NO_PROXY`, damit gRPC nicht über den Unternehmens-Proxy läuft). Die Sandbox bleibt unberührt, sie spricht nur mit `proxy`.
- `ORCHIVIO_CA_FILE`: Pfad eines PEM-Bundles auf dem Rechner (unter `$HOME`, weil die Podman-Machine nur das Home-Verzeichnis einbindet; die Datei muss existieren). Compose bindet es schreibgeschützt als `/etc/orchivio/ca.pem` in `adapter-jira-cloud`, `adapter-jira-dc`, `adapter-gitlab`, `proxy` und `worker` ein und setzt dort `SSL_CERT_FILE=/etc/orchivio/ca.pem` (im Worker auch `GIT_SSL_CAINFO`). Ohne `ORCHIVIO_CA_FILE` wird `/dev/null` eingebunden und `SSL_CERT_FILE` bleibt leer, die Images nutzen ihre eigenen CAs.
- Vorrang: `ORCHIVIO_CA_FILE` bestimmt `SSL_CERT_FILE`; ein `SSL_CERT_FILE` aus `.env` oder Umgebung wird nicht mehr durchgereicht (es war ein Pfad auf dem Rechner, nicht im Container). Wer einen anderen Pfad braucht, setzt ihn in einer `compose.override.yaml`; die gewinnt. `SSL_CERT_DIR` wird weiter durchgereicht.
- `SSL_CERT_FILE` (Go) und `GIT_SSL_CAINFO` (git) **ersetzen** die System-CAs. Das Bundle muss daher Unternehmens-CA **und** System-CAs enthalten, sonst scheitern z. B. Aufrufe an `api.anthropic.com` (siehe Arbeitslaptop, Schritt 4).
- Grenze `proxy`: Die Anthropic-Aufrufe (Reverse-Proxy, Go-Standardtransport) gehen über `HTTPS_PROXY`. CONNECT-Tunnel der Sandbox (Allowlist) wählt `proxy` dagegen direkt an; ohne direkten Internetzugang funktionieren sie nicht.
- Grenze `worker` (`git` über HTTPS gegen GitLab): Der Worker entfernt für `git` alle geerbten `GIT_*`-Variablen (`internal/gitenv` in worker-podman, auch in 0.1.0), `GIT_SSL_CAINFO` kommt also nicht an. Das `git` im Image nutzt libcurl mit GnuTLS und ignoriert `SSL_CERT_FILE`. Proxy-Variablen kommen an. Bis worker-podman `GIT_SSL_CAINFO` durchlässt, das Bundle in einer `dev/compose.override.yaml` über die System-CAs des Workers legen (das Bundle enthält sie ja):

  ```yaml
  services:
    worker:
      volumes:
        - ${ORCHIVIO_CA_FILE}:/etc/ssl/certs/ca-certificates.crt:ro
  ```

  `run-all.sh` und `stop-all.sh` lesen nur `dev/compose.yaml`; mit Override von Hand starten: `podman compose -f dev/compose.yaml -f dev/compose.override.yaml up -d` (Variablen wie in `run-all.sh` setzen) oder die Zeile vorübergehend in `dev/compose.yaml` ergänzen. Nicht im Test verifiziert.

### Checkliste (je Jira Data Center, Jira Cloud, GitLab CE/SaaS)

- [ ] Verbinden: `seed.sh` bzw. Formular-Endpoint; `GET /tenants/beta/connections` zeigt `active`.
- [ ] Projekt mit `statusMap` angelegt.
- [ ] Ticket dem Orchivio-Nutzer zuweisen: Run startet, Status wechselt auf „In Progress“.
- [ ] Run endet mit einem Merge Request; Ticket-Status wechselt auf „In Review“.
- [ ] Kommentar im MR mit Erwähnung von Orchivio: Antwort bzw. Folge-Run.
- [ ] Folge-Run auf demselben Branch: neuer Commit im bestehenden MR.
- [ ] Tokens widerrufen (Verbindung löschen): Die Projekt-Tokenliste in GitLab ist leer.
- [ ] Ergebnis (Datum, Instanz, Auffälligkeiten) hier eintragen.

## Arbeitslaptop: Jira Data Center und GitLab

Für einen Rechner mit nur Jira Data Center und selbst betriebenem GitLab hinter einem HTTP-Proxy mit eigener CA. Linear und GitHub sind nicht nötig. Schritte, die hier mit „nicht verifiziert“ markiert sind, sind nicht auf einem echten Firmenrechner getestet.

1. **Firmenrichtlinie klären.** Orchivio schickt Ticket-Text und Quellcode des Repos an Anthropic (Claude). Vorher klären, ob das für das Projekt erlaubt ist.

2. **Voraussetzungen.** Podman mit Podman-Machine (mindestens 8 GB Speicher), `podman compose` (getestet mit dem Provider docker-compose v2.33; `stop-all.sh` nutzt `COMPOSE_PROFILES='*'`), `git`, `jq`, `curl`, `openssl`. Go ist nur für die Tests nötig.

3. **Podman-Machine: Proxy und CA.** Die Images kommen von `ghcr.io` und `docker.io`; das Ziehen passiert in der VM, sie braucht also Proxy und Unternehmens-CA.
   - Proxy: Die Variablen vor `podman machine init` in der Shell setzen. Podman übernimmt `HTTP_PROXY`, `HTTPS_PROXY` und `NO_PROXY` aus der Shell in die VM. Nicht verifiziert: ob ein späteres `podman machine stop && podman machine start` geänderte Werte übernimmt; im Zweifel die VM neu anlegen oder die Werte in der VM (`podman machine ssh`) unter `/etc/systemd/system.conf.d/` und `/etc/profile.d/` prüfen:

     ```sh
     export HTTP_PROXY=http://proxy.example.com:3128 HTTPS_PROXY=http://proxy.example.com:3128
     export NO_PROXY=localhost,127.0.0.1,.example.com
     podman machine init --memory 8192
     podman machine start
     podman machine ssh 'env | grep -i _proxy'   # prüfen
     ```

     Läuft der Proxy lokal auf dem Mac (z. B. `127.0.0.1:3128`), muss die VM ihn über `host.containers.internal` erreichen; ob Podman das umschreibt, ist nicht verifiziert.
   - CA in der VM vertrauen (Fedora CoreOS; das Home-Verzeichnis ist in der VM unter gleichem Pfad eingebunden, `/etc` bleibt über Neustarts erhalten, geht aber bei `podman machine rm` verloren):

     ```sh
     podman machine ssh "sudo cp $HOME/.orchivio/corp-ca.pem /etc/pki/ca-trust/source/anchors/corp-ca.pem && sudo update-ca-trust"
     podman pull ghcr.io/orchivio/orchestrator:0.1.0   # prüfen
     ```

4. **CA-Bundle bauen.** Die Unternehmens-CA als PEM besorgen (IT oder Schlüsselbund, z. B. `security find-certificate -a -c "<Name der CA>" -p /Library/Keychains/System.keychain > ~/.orchivio/corp-ca.pem`) und mit den System-CAs zusammenfügen, weil `SSL_CERT_FILE` und `GIT_SSL_CAINFO` die System-CAs ersetzen:

   ```sh
   mkdir -p ~/.orchivio
   cat ~/.orchivio/corp-ca.pem /etc/ssl/cert.pem > ~/.orchivio/ca-bundle.pem
   openssl x509 -in ~/.orchivio/corp-ca.pem -noout -subject -enddate      # CA lesbar?
   curl -sS -o /dev/null -w '%{http_code}\n' --cacert ~/.orchivio/ca-bundle.pem https://gitlab.example.com  # Bundle passt?
   ```

5. **Nur das Orchestrator-Repo klonen.** Es enthält `dev/`; alle Dienste kommen als Images.

   ```sh
   git clone https://github.com/orchivio/orchestrator.git && cd orchestrator
   cp dev/env.example dev/.env
   ```

6. **`dev/.env` ausfüllen.** Die Linear-/GitHub-Werte aus `env.example` können leer bleiben oder entfernt werden.

   ```sh
   ORCHIVIO_SECRET_KEY=...            # openssl rand -base64 32
   ORCHIVIO_BOOTSTRAP_TOKEN=orv_...   # siehe env.example
   ANTHROPIC_API_KEY=sk-ant-...       # oder CLAUDE_CODE_OAUTH_TOKEN
   ORCHIVIO_VERSION=0.1.0
   COMPOSE_PROFILES=jira-gitlab
   ORCHIVIO_ADAPTERS=jira-dc=adapter-jira-dc:7302,gitlab=adapter-gitlab:7401
   JIRA_URL=https://jira.example.com
   GITLAB_URL=https://gitlab.example.com
   HTTPS_PROXY=http://proxy.example.com:3128
   HTTP_PROXY=http://proxy.example.com:3128
   NO_PROXY=localhost,127.0.0.1,.example.com   # interne Hosts (Jira, GitLab) meist hier
   ORCHIVIO_CA_FILE=/Users/<du>/.orchivio/ca-bundle.pem
   # Seed: Mandant beta (ORCHIVIO_SEED_TENANT=acme kann stehen bleiben, acme wird übersprungen)
   ORCHIVIO_SEED_EMAIL=du@example.com
   ORCHIVIO_SEED_JIRA_ADAPTER=jira-dc
   ORCHIVIO_SEED_JIRA_TOKEN=...       # PAT des Jira-Nutzers von Orchivio (Schritt 8)
   ORCHIVIO_SEED_JIRA_KEY=ABC         # Jira-Projektschlüssel
   ORCHIVIO_SEED_GITLAB_PATH=group/project
   ORCHIVIO_SEED_GITLAB_TOKEN=glpat-...   # Project Access Token (Schritt 7)
   ORCHIVIO_SEED_GITLAB_REPO=group/project
   ORCHIVIO_SEED_JIRA_USER=id:JIRAUSER12345   # dein DC-Benutzerschlüssel
   ORCHIVIO_SEED_GITLAB_USER=id:12345         # deine GitLab-User-ID
   ```

   Den DC-Benutzerschlüssel liefert `curl -H "Authorization: Bearer <dein PAT>" $JIRA_URL/rest/api/2/myself | jq -r .key`, die GitLab-User-ID `curl -H "PRIVATE-TOKEN: <dein PAT>" $GITLAB_URL/api/v4/user | jq .id`.

7. **GitLab: Project Access Token.** Im Projekt Settings, Access Tokens: Rolle **Maintainer**, Scope **`api`**. GitLab legt dafür einen eigenen Bot-Nutzer an, der nur Mitglied dieses einen Projekts ist; damit ist Orchivio auf das Projekt beschränkt. Nicht verifiziert ist, ob dieser Projekt-Bot selbst weitere Project Access Tokens anlegen darf (die Instanz kann das per Einstellung oder Version verbieten). Das ist die erste Prüfung nach dem Start: `GET /tenants/beta/connections` zeigt `active` und ein Run kann pushen; scheitert es, die Logs von `adapter-gitlab` lesen.

8. **Jira: eigener technischer Nutzer empfohlen.** Orchivio ignoriert alles, was sein eigener Nutzer tut (Schutz vor Schleifen). Mit einem eigenen technischen Nutzer (und dessen Personal Access Token) kannst du selbst Tickets zuweisen und kommentieren. Mit deinem eigenen PAT handelt Orchivio als du: Dann muss eine Kollegin oder ein Kollege den Test auslösen (zuweisen, erwähnen) und in den Rollen stehen.

9. **Starten und seeden.**

   ```sh
   dev/run-all.sh       # zieht die Images, startet, ruft dev/seed.sh auf (legt beta an)
   curl -H "Authorization: Bearer $ORCHIVIO_BOOTSTRAP_TOKEN" localhost:8088/api/v1/tenants/beta/connections
   podman compose -f dev/compose.yaml logs -f adapter-jira-dc adapter-gitlab worker proxy
   dev/stop-all.sh      # stoppt alles, die Datenbank bleibt
   ```

   `adapter-jira-cloud` läuft im Profil mit, wird aber ohne Eintrag in `ORCHIVIO_ADAPTERS` nicht genutzt. Beim Klonen und Pushen durch den Worker die Grenze unter [Proxy und eigene CA](#proxy-und-eigene-ca) beachten.

10. **Test durchgehen:** [Checkliste](#checkliste-je-jira-data-center-jira-cloud-gitlab-cesaas) für Jira Data Center und GitLab abhaken.

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
