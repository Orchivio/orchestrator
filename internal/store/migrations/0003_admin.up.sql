CREATE TABLE tenants (
    slug             text        PRIMARY KEY, -- also for deleted rows: a slug is never reused
    display_name     text        NOT NULL,
    tracker_adapter  text        NOT NULL,
    code_host_adapter text      NOT NULL,
    version          bigint      NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL,
    updated_at       timestamptz NOT NULL,
    deleted_at       timestamptz
);

CREATE TABLE profiles (
    id                   uuid        PRIMARY KEY,
    tenant_slug          text        NOT NULL REFERENCES tenants (slug),
    name                 text        NOT NULL,
    runtime              text        NOT NULL,
    model                text        NOT NULL,
    instructions         text        NOT NULL,
    max_duration_seconds bigint      NOT NULL,
    version              bigint      NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    deleted_at           timestamptz
);
CREATE UNIQUE INDEX profiles_live_name ON profiles (tenant_slug, name) WHERE deleted_at IS NULL;

CREATE TABLE projects (
    id              uuid        PRIMARY KEY,
    tenant_slug     text        NOT NULL REFERENCES tenants (slug),
    tracker_adapter text        NOT NULL,
    tracker_key     text        NOT NULL,
    name            text        NOT NULL,
    profile_id      uuid        NOT NULL REFERENCES profiles (id),
    spec            jsonb       NOT NULL, -- repos, classifier, roles, maxDiffLines, milestones
    version         bigint      NOT NULL DEFAULT 1,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    deleted_at      timestamptz,
    UNIQUE (tracker_adapter, tracker_key) -- unfiltered: a key is never reused
);

ALTER TABLE runs ADD COLUMN project_id uuid;

CREATE TABLE tokens (
    id          uuid        PRIMARY KEY,
    name        text        NOT NULL,
    prefix      text        NOT NULL,
    hash        bytea       NOT NULL UNIQUE, -- SHA-256 of the plaintext; the plaintext is never stored
    scope       text        NOT NULL,
    tenant_slug text        REFERENCES tenants (slug),
    role        text        NOT NULL,
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz
);
