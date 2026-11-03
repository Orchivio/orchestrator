CREATE TABLE inbound_events (
    source      text        NOT NULL,
    external_id text        NOT NULL,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (source, external_id)
);

CREATE TABLE conversations (
    id         uuid        PRIMARY KEY,
    tenant_id  text        NOT NULL,
    project    text        NOT NULL,
    ticket_ref text        NOT NULL,
    created_at timestamptz NOT NULL,
    UNIQUE (tenant_id, ticket_ref)
);

CREATE TABLE runs (
    id              uuid        PRIMARY KEY,
    tenant_id       text        NOT NULL,
    conversation_id uuid        NOT NULL REFERENCES conversations (id),
    project         text        NOT NULL,
    ticket_ref      text        NOT NULL,
    kind            text        NOT NULL,
    state           text        NOT NULL,
    repos           jsonb       NOT NULL DEFAULT '[]',
    branch          text        NOT NULL DEFAULT '',
    prompt          text        NOT NULL DEFAULT '',
    result          text        NOT NULL DEFAULT '',
    pr_urls         jsonb       NOT NULL DEFAULT '[]',
    session_id      text        NOT NULL DEFAULT '',
    lease_owner     text        NOT NULL DEFAULT '',
    lease_until     timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX runs_state_created_at ON runs (state, created_at);
CREATE INDEX runs_conversation ON runs (conversation_id, created_at);

CREATE TABLE inputs (
    id              uuid        PRIMARY KEY,
    tenant_id       text        NOT NULL,
    conversation_id uuid        NOT NULL REFERENCES conversations (id),
    actor_email     text        NOT NULL,
    body            text        NOT NULL,
    trigger         text        NOT NULL,
    run_id          uuid        REFERENCES runs (id),
    created_at      timestamptz NOT NULL
);
CREATE INDEX inputs_conversation ON inputs (conversation_id, created_at);

CREATE TABLE events (
    id        bigserial   PRIMARY KEY,
    tenant_id text        NOT NULL,
    run_id    uuid        NOT NULL REFERENCES runs (id),
    type      text        NOT NULL,
    payload   jsonb       NOT NULL,
    at        timestamptz NOT NULL
);
CREATE INDEX events_run ON events (run_id, id);
