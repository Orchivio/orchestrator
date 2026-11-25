ALTER TABLE projects DROP CONSTRAINT projects_tenant_slug_tracker_key_key;
ALTER TABLE projects ADD CONSTRAINT projects_tracker_adapter_tracker_key_key UNIQUE (tracker_adapter, tracker_key);

DELETE FROM inbound_events a USING inbound_events b
WHERE a.source = b.source AND a.external_id = b.external_id AND a.ctid > b.ctid;
ALTER TABLE inbound_events DROP CONSTRAINT inbound_events_pkey;
ALTER TABLE inbound_events DROP COLUMN tenant_id;
ALTER TABLE inbound_events ADD PRIMARY KEY (source, external_id);

DROP TABLE oauth_states;
DROP TABLE connections;
