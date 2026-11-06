# Lokaler Ende-zu-Ende-Test

Diese Anleitung startet Orchivio komplett auf deinem Rechner (Server, Worker, Linear- und GitHub-Adapter, Postgres, Proxy) und prüft es gegen echtes Linear, echtes GitHub und echtes Claude.

Ohne externe Dienste läuft der automatische Rauchtest:

```sh
export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')
export TESTCONTAINERS_RYUK_DISABLED=true
go test -tags e2e ./internal/e2e/
```

## Voraussetzungen

- Go 1.26
- Podman, die Podman-Machine läuft (`podman machine start`)
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

1. Lege in deinem Workspace einen technischen Nutzer **„Orchivio“** an (eigener Account, Mitglied im Team). Er darf nicht dein eigener Account sein: Kommentare des Nutzers, dem der API-Key gehört, ignoriert der Adapter (sonst würde Orchivio auf seine eigenen Kommentare reagieren). Mit deinem eigenen Key passiert auf deine Kommentare also nichts.
2. Als dieser Nutzer: Settings, Account, Security & access, **Personal API keys**, neuen Key erzeugen. Er kommt als `LINEAR_TOKEN_ACME` in die `.env`.
3. Den **Team-Key** (z. B. `ORC`) findest du in den Team-Einstellungen. Er gehört in `LINEAR_TEAM_KEYS` und in `trackerKey` der Config.
4. Bedienung: In einem Ticket des Teams `@Orchivio` im Kommentar erwähnen, oder das Ticket dem Nutzer Orchivio zuweisen.

## Konfiguration anpassen

`dev/config/acme.yaml` bearbeiten:

- `roles.questions` und `roles.tasks`: deine Linear-E-Mail-Adresse statt `you@example.com`. Wer nicht gelistet ist, bekommt bei Erwähnung oder Zuweisung den Hinweis „Rolle fehlt“ und löst keinen Run aus; einfache Kommentare ohne Erwähnung werden ignoriert.
- `projects[].trackerKey`: dein Team-Key.
- `repos[].slug`, `classifier.defaultRepo`, `classifier.repoLabels`: dein Test-Repo (`owner/name`).
- `profiles.*.model` (`model:`): die Modell-ID gegen die Modelle prüfen, die dein Key bzw. dein Abo nutzen darf. Ein Alias wie `sonnet` funktioniert mit Claude Code.

## Starten und Stoppen

```sh
dev/run-all.sh   # Postgres, Proxy, Build, alle Prozesse im Hintergrund
dev/stop-all.sh  # beendet die Prozesse und den Proxy-Container
```

Der erste Start baut die Sandbox-Images und dauert einige Minuten. Die Ports 5432, 7070, 7101, 7102, 7201 und 8088 müssen frei sein. Alle außer dem Webhook (`:7102`) lauschen nur auf `127.0.0.1`: gRPC läuft unverschlüsselt und ohne Authentifizierung. Ein Worker auf einem anderen Rechner braucht eine explizite Adresse (`ORCHIVIO_GRPC_ADDR`) und vorher mTLS. Ein zweiter Start bricht ab, solange Prozesse des ersten laufen; dann zuerst `dev/stop-all.sh` ausführen.

`run-all.sh` meldet fehlende Variablen mit Namen (nie mit Werten) und wartet bis zu 30 Sekunden, bis der Server antwortet. Postgres bleibt nach dem Stoppen laufen (`podman compose -f dev/compose.yaml down` beendet es).

## Logs und Timelines

- Logs: `dev/logs/<name>.log` (`server`, `worker`, `linear-adapter`, `github-adapter`), PIDs in `dev/logs/pids`.
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
- Die Linear-GraphQL-Formen sind nicht gegen die Live-API verifiziert. Passiert nichts, zuerst `dev/logs/linear-adapter.log` auf GraphQL-Fehler prüfen.
