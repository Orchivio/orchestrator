-- name: MarkInbound :execrows
INSERT INTO inbound_events (source, external_id, received_at)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

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
