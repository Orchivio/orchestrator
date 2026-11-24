-- name: MarkInbound :execrows
INSERT INTO inbound_events (tenant_id, source, external_id, received_at)
VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING;

-- name: FindConversation :one
SELECT * FROM conversations WHERE tenant_id = $1 AND ticket_ref = $2;

-- name: GetConversation :one
SELECT * FROM conversations WHERE id = $1;

-- name: CreateConversation :exec
INSERT INTO conversations (id, tenant_id, project, ticket_ref, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: AddInput :exec
INSERT INTO inputs (id, tenant_id, conversation_id, actor_email, body, trigger, run_id, created_at, from_agent)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: OpenInputs :many
SELECT * FROM inputs WHERE conversation_id = $1 AND run_id IS NULL
ORDER BY created_at, id;

-- name: ClaimInputs :exec
UPDATE inputs SET run_id = $2 WHERE conversation_id = $1 AND run_id IS NULL;

-- name: ListInputs :many
SELECT * FROM inputs WHERE conversation_id = $1 ORDER BY created_at, id;

-- name: ListFinishedRuns :many
-- A waiting run's result is its question, already a from_agent input; only final results count.
SELECT * FROM runs WHERE conversation_id = $1 AND result <> ''
  AND state IN ('succeeded', 'failed', 'cancelled') ORDER BY created_at, id;

-- name: ActiveRun :one
SELECT * FROM runs
WHERE conversation_id = $1
  AND state IN ('classifying', 'queued', 'running', 'pushing', 'waiting_for_input')
ORDER BY created_at DESC, id LIMIT 1;

-- name: LastRun :one
SELECT * FROM runs WHERE conversation_id = $1 ORDER BY created_at DESC, id LIMIT 1;

-- name: GetRun :one
SELECT * FROM runs WHERE id = $1;

-- name: CreateRun :exec
INSERT INTO runs (id, tenant_id, conversation_id, project, project_id, ticket_ref, kind, state, repos, branch,
                  prompt, result, pr_urls, session_id, lease_owner, lease_until, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18);

-- name: UpdateRun :execrows
UPDATE runs SET state = $2, repos = $3, branch = $4, prompt = $5, result = $6, pr_urls = $7,
                session_id = $8, lease_owner = $9, lease_until = $10, updated_at = $11, kind = $12
WHERE id = $1;

-- name: LeaseNext :one
-- ponytail: FIFO ohne Fairness, faire Verteilung pro Mandant in ORC-39
UPDATE runs SET state='running', lease_owner=$1, lease_until=$2, updated_at=$3
WHERE id = (SELECT id FROM runs
            WHERE state='queued' OR (state='running' AND lease_until < $3)
            ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT 1)
RETURNING *;

-- name: AppendEvent :exec
INSERT INTO events (tenant_id, run_id, type, payload, at) VALUES ($1, $2, $3, $4, $5);

-- name: ListEvents :many
SELECT * FROM events WHERE run_id = $1 ORDER BY id;

-- name: ListTenants :many
SELECT * FROM tenants WHERE deleted_at IS NULL ORDER BY slug;

-- name: GetTenant :one
SELECT * FROM tenants WHERE slug = $1 AND deleted_at IS NULL;

-- name: CreateTenant :one
INSERT INTO tenants (slug, display_name, tracker_adapter, code_host_adapter, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $5) RETURNING *;

-- name: UpdateTenant :one
UPDATE tenants SET display_name = $2, tracker_adapter = $3, code_host_adapter = $4,
                   version = version + 1, updated_at = $5
WHERE slug = $1 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version))
RETURNING *;

-- name: DeleteTenant :execrows
UPDATE tenants SET deleted_at = $2, updated_at = $2, version = version + 1
WHERE slug = $1 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version));

-- name: CountUnfinishedRunsOfTenant :one
SELECT count(*) FROM runs WHERE tenant_id = $1 AND state NOT IN ('succeeded', 'failed', 'cancelled');

-- name: CountUnfinishedRunsOfProject :one
SELECT count(*) FROM runs WHERE tenant_id = $1 AND project = $2 AND state NOT IN ('succeeded', 'failed', 'cancelled');

-- name: ListProfiles :many
SELECT * FROM profiles WHERE tenant_slug = $1 AND deleted_at IS NULL ORDER BY name;

-- name: GetProfile :one
SELECT * FROM profiles WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL;

-- name: CreateProfile :one
INSERT INTO profiles (id, tenant_slug, name, runtime, model, instructions, max_duration_seconds, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING *;

-- name: UpdateProfile :one
UPDATE profiles SET name = $3, runtime = $4, model = $5, instructions = $6, max_duration_seconds = $7,
                    version = version + 1, updated_at = $8
WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version))
RETURNING *;

-- name: DeleteProfile :execrows
UPDATE profiles SET deleted_at = $3, updated_at = $3, version = version + 1
WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version));

-- name: CountLiveProjectsOfProfile :one
SELECT count(*) FROM projects WHERE profile_id = $1 AND deleted_at IS NULL;

-- name: ListProjects :many
SELECT * FROM projects WHERE tenant_slug = $1 AND deleted_at IS NULL ORDER BY tracker_key;

-- name: GetProject :one
SELECT * FROM projects WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL;

-- name: CreateProject :one
INSERT INTO projects (id, tenant_slug, tracker_adapter, tracker_key, name, profile_id, spec, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING *;

-- name: UpdateProject :one
UPDATE projects SET name = $3, profile_id = $4, spec = $5, version = version + 1, updated_at = $6
WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version))
RETURNING *;

-- name: DeleteProject :execrows
UPDATE projects SET deleted_at = $3, updated_at = $3, version = version + 1
WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL AND (sqlc.arg(if_version)::bigint = 0 OR version = sqlc.arg(if_version));

-- name: CreateToken :one
INSERT INTO tokens (id, name, prefix, hash, scope, tenant_slug, role, created_at, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING *;

-- name: ListTokens :many
SELECT * FROM tokens ORDER BY created_at DESC, id;

-- name: RevokeToken :execrows
UPDATE tokens SET revoked_at = coalesce(revoked_at, $2) WHERE id = $1;

-- name: TokenByHash :one
SELECT * FROM tokens WHERE hash = $1;

-- name: CountTokens :one
SELECT count(*) FROM tokens;

-- name: ListConnections :many
SELECT * FROM connections WHERE tenant_slug = $1 AND deleted_at IS NULL ORDER BY created_at, id;

-- name: ListAllActiveConnections :many
SELECT * FROM connections WHERE deleted_at IS NULL AND status = 'active' ORDER BY created_at, id;

-- name: GetConnection :one
SELECT * FROM connections WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL;

-- name: GetConnectionCredential :one
SELECT key_id, credential FROM connections WHERE id = $1 AND deleted_at IS NULL;

-- Reconnect keeps id and created_at; no row returned = another tenant holds the external id.
-- name: SaveConnection :one
INSERT INTO connections (id, tenant_slug, kind, adapter, external_id, label, status, key_id, credential, created_at, updated_at, refresh_after)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10, $11)
ON CONFLICT (adapter, external_id) WHERE deleted_at IS NULL DO UPDATE
SET label = EXCLUDED.label, status = EXCLUDED.status, key_id = EXCLUDED.key_id, credential = EXCLUDED.credential,
    refresh_after = EXCLUDED.refresh_after, updated_at = EXCLUDED.updated_at
WHERE connections.tenant_slug = EXCLUDED.tenant_slug
RETURNING *;

-- name: DeleteOtherTrackerConnections :exec
UPDATE connections SET deleted_at = sqlc.arg(deleted_at), updated_at = sqlc.arg(deleted_at)
WHERE tenant_slug = sqlc.arg(tenant_slug) AND kind = 'tracker' AND deleted_at IS NULL
  AND NOT (adapter = sqlc.arg(adapter) AND external_id = sqlc.arg(external_id));

-- name: UpdateConnectionCredential :execrows
UPDATE connections SET key_id = $2, credential = $3, refresh_after = $4, status = $5, updated_at = $6
WHERE id = $1 AND deleted_at IS NULL;

-- name: DeleteConnection :execrows
UPDATE connections SET deleted_at = $3, updated_at = $3 WHERE tenant_slug = $1 AND id = $2 AND deleted_at IS NULL;

-- name: ClaimDueConnections :many
UPDATE connections SET refresh_after = sqlc.arg(lease_until)::timestamptz
WHERE id IN (
    SELECT c.id FROM connections c
    WHERE c.deleted_at IS NULL AND c.status = 'active' AND c.refresh_after <= sqlc.arg(now)::timestamptz
    ORDER BY c.refresh_after FOR UPDATE SKIP LOCKED)
RETURNING *;

-- name: CreateOAuthState :exec
INSERT INTO oauth_states (hash, tenant_slug, adapter, kind, created_at, expires_at) VALUES ($1, $2, $3, $4, $5, $6);

-- name: ConsumeOAuthState :one
UPDATE oauth_states SET used_at = sqlc.arg(now)::timestamptz
WHERE hash = $1 AND used_at IS NULL AND expires_at > sqlc.arg(now)::timestamptz
RETURNING tenant_slug, adapter, kind;
