CREATE TABLE connections (
    id            uuid        PRIMARY KEY,
    tenant_slug   text        NOT NULL REFERENCES tenants (slug),
    kind          text        NOT NULL,
    adapter       text        NOT NULL,
    external_id   text        NOT NULL,
    label         text        NOT NULL,
    status        text        NOT NULL,
    key_id        text        NOT NULL,
    credential    bytea       NOT NULL, -- AES-GCM ciphertext, never plaintext
    refresh_after timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    deleted_at    timestamptz
);
CREATE UNIQUE INDEX connections_live_external ON connections (adapter, external_id) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX connections_live_tracker ON connections (tenant_slug) WHERE kind = 'tracker' AND deleted_at IS NULL;

CREATE TABLE oauth_states (
    hash        bytea       PRIMARY KEY, -- SHA-256 of the state; the state itself is never stored
    tenant_slug text        NOT NULL REFERENCES tenants (slug),
    adapter     text        NOT NULL,
    kind        text        NOT NULL,
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz
);

ALTER TABLE inbound_events ADD COLUMN tenant_id text NOT NULL DEFAULT '';
ALTER TABLE inbound_events DROP CONSTRAINT inbound_events_pkey;
ALTER TABLE inbound_events ADD PRIMARY KEY (tenant_id, source, external_id);

ALTER TABLE projects DROP CONSTRAINT projects_tracker_adapter_tracker_key_key;
ALTER TABLE projects ADD CONSTRAINT projects_tenant_slug_tracker_key_key UNIQUE (tenant_slug, tracker_key); -- unfiltered: never reused in a tenant
