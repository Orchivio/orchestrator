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

## GitHub-App einrichten

1. GitHub, Settings, Developer settings, GitHub Apps, **New GitHub App**.
2. Name frei wählen, Homepage-URL beliebig. **Webhook** deaktivieren (kein Haken bei „Active“), es werden keine Webhooks benötigt.
3. Repository permissions:
   - Contents: Read and write
   - Pull requests: Read and write
   - Metadata: Read-only
4. „Where can this GitHub App be installed?“: Only on this account.
5. App erstellen. Die **App ID** steht oben auf der Seite der App (Abschnitt „About“). Sie kommt als `GITHUB_APP_ID` in die `.env`.
6. Weiter unten **Generate a private key**. Die heruntergeladene `.pem`-Datei enthält den Schlüssel.
7. **Install App** (linke Seitenleiste), dein Konto wählen, „Only select repositories“ und **nur das Test-Repo** auswählen.
8. Den Inhalt der PEM-Datei in `dev/.env` als mehrzeiligen Wert in Anführungszeichen eintragen:

   ```
   GITHUB_APP_PRIVATE_KEY="-----BEGIN RSA PRIVATE KEY-----
   MIIE...
   -----END RSA PRIVATE KEY-----"
   ```

## Linear einrichten

Orchivio läuft in Linear als **App-User** einer OAuth-App: eigener Nutzer, erwähnbar und zuweisbar, kostet keinen Platz. Du brauchst Admin-Rechte im Workspace.

1. Settings, API, **OAuth applications**, neue App anlegen (<https://linear.app/settings/api/applications/new>). Name **Orchivio** (wird zum Anzeigenamen des App-Users). Callback-URL ist Pflicht, wird aber nicht genutzt: `http://localhost/callback`. Webhooks aus.
2. In der App **Client credentials** aktivieren. Client-ID und Client-Secret kommen als `LINEAR_CLIENT_ID` und `LINEAR_CLIENT_SECRET` in die `.env`.
3. Beim Start holt der Adapter selbst ein Token (`client_credentials`, Scopes `read,write,app:assignable,app:mentionable`, 30 Tage gültig) und erneuert es rechtzeitig. Damit erscheint der App-User im Workspace. Er hat Zugriff auf alle öffentlichen Teams.
4. Den **Team-Key** (z. B. `ORC`) findest du in den Team-Einstellungen. Er gehört in `LINEAR_TEAM_KEYS` und in `trackerKey` der Config.
5. Bedienung: In einem Ticket des Teams `@Orchivio` im Kommentar erwähnen, oder das Ticket an Orchivio delegieren (Zuweisen-Menü, Orchivio auswählen).

Alternative ohne OAuth-App: eigener Nutzer „Orchivio“ (zweiter Account, kostet einen Platz) mit persönlichem API-Key als `LINEAR_TOKEN_ACME` und `LINEAR_API_KEY_REF`, `LINEAR_CLIENT_ID` leer lassen. Nicht der eigene Account: Kommentare des Key-Besitzers ignoriert der Adapter.

## Konfiguration anpassen

`dev/config/acme.yaml` bearbeiten:

- `roles.questions` und `roles.tasks`: deine Linear-E-Mail-Adresse statt `you@example.com`. Wer nicht gelistet ist, bekommt bei Erwähnung oder Zuweisung den Hinweis „Rolle fehlt“ und löst keinen Run aus; einfache Kommentare ohne Erwähnung werden ignoriert.
- `projects[].trackerKey`: dein Team-Key.
- `repos[].slug`, `classifier.defaultRepo`, `classifier.repoLabels`: dein Test-Repo (`owner/name`).
- `profiles.*.model` (`model:`): die Modell-ID gegen die Modelle prüfen, die dein Key bzw. dein Abo nutzen darf. Ein Alias wie `sonnet` funktioniert mit Claude Code.

## Starten und Stoppen

```sh
dev/run-all.sh                         # zieht die Images (Tag main) und startet alles
ORCHIVIO_VERSION=1.2.0 dev/run-all.sh  # eine veröffentlichte Version
dev/stop-all.sh                        # stoppt alles, die Datenbank bleibt im Volume
```

`run-all.sh` meldet fehlende Variablen mit Namen (nie mit Werten). Nur die Timeline (`127.0.0.1:8088`) ist vom Rechner aus erreichbar; gRPC bleibt im Compose-Netz (unverschlüsselt, ohne Authentifizierung, mTLS vor entfernten Workern). Der Worker startet die Sandboxen über den Podman-Socket der Machine; sein Arbeitsverzeichnis liegt unter `~/.orchivio/work`, weil die Machine das Home-Verzeichnis unter gleichem Pfad einbindet. Mit `ORCHIVIO_PULL=never` nutzt `run-all.sh` lokal vorhandene Images.

## Bauen und Veröffentlichen

Jedes Repo baut in GitHub Actions (`.github/workflows/ci.yml`), trunk-based:

- Pull Request: Format, `go vet`, Tests (im Orchestrator auch der Ende-zu-Ende-Test).
- Push auf `main`: zusätzlich Images für amd64 und arm64 nach `ghcr.io/orchivio/<repo>` mit den Tags `main` und `sha-<commit>`.
- Tag `vX.Y.Z`: Images `X.Y.Z`, `X.Y` und `latest`.
- Contract: Tag `contract/vX.Y.Z` im Orchestrator-Repo veröffentlicht das Go-Modul `github.com/orchivio/orchestrator/contract`; Dependabot schlägt das Update in den anderen Repos vor. Lokal überschreibt `go.work` die Version, so dass Änderungen am Contract sofort überall greifen.

## Logs und Timelines

- Logs: `podman compose -f dev/compose.yaml logs -f [orchestrator|worker|adapter-linear|adapter-github|proxy]`.
- Timeline eines Runs als JSON: `http://localhost:8088/runs/<run-id>`. Der Link steht in jedem Meilenstein-Kommentar im Ticket.

## Manuelle Checkliste

- [ ] Linear-Ticket im Team `ORC`, Kommentar „@Orchivio Was steht in der README?“ ergibt eine Antwort im Ticket; der Timeline-Link öffnet JSON.
- [ ] Ticket Orchivio zuweisen, Kommentar „Füge einen Abschnitt Kontakt in die README ein“ ergibt einen PR in `soemii/orchivio-playground` auf einem Branch `orchivio/orc-…`.
- [ ] Eine nicht konfigurierte Person erwähnt `@Orchivio` oder weist das Ticket Orchivio zu: Das ergibt den Hinweis „Rolle fehlt“, es startet kein Run. Ein einfacher Kommentar ohne Erwähnung von ihr wird still ignoriert (kein Hinweis).
- [ ] Unklare Aufgabe ergibt eine Rückfrage im Ticket; deine Antwort startet einen neuen Lauf mit Verlauf.

## Bekannte Grenzen des Durchstichs

- Keine OIDC-Absicherung der Timeline (nur an `127.0.0.1` gebunden).
- gRPC ohne TLS und ohne Authentifizierung (Server, Adapter standardmäßig nur an `127.0.0.1` gebunden); mTLS vor entfernten Workern.
- Keine Kommentare im PR-Review.
- Kein Resume und keine Snapshots: Jede Antwort startet einen neuen Lauf.
- Nur Polling, keine Webhooks.
- Die Linear-GraphQL-Formen sind nicht gegen die Live-API verifiziert. Passiert nichts, zuerst die Logs von `adapter-linear` auf GraphQL-Fehler prüfen.
