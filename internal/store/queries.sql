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
INSERT INTO inputs (id, tenant_id, conversation_id, actor_email, body, trigger, run_id, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: OpenInputs :many
SELECT * FROM inputs WHERE conversation_id = $1 AND run_id IS NULL
ORDER BY created_at, id;

-- name: ClaimInputs :exec
UPDATE inputs SET run_id = $2 WHERE conversation_id = $1 AND run_id IS NULL;

-- name: ListInputs :many
SELECT * FROM inputs WHERE conversation_id = $1 ORDER BY created_at, id;

-- name: ListFinishedRuns :many
SELECT * FROM runs WHERE conversation_id = $1 AND result <> '' ORDER BY created_at, id;

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
INSERT INTO runs (id, tenant_id, conversation_id, project, ticket_ref, kind, state, repos, branch,
                  prompt, result, pr_urls, session_id, lease_owner, lease_until, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- name: UpdateRun :execrows
UPDATE runs SET state = $2, repos = $3, branch = $4, prompt = $5, result = $6, pr_urls = $7,
                session_id = $8, lease_owner = $9, lease_until = $10, updated_at = $11
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
