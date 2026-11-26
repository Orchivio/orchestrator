-- Review comments look up the task run of a branch per tenant.
CREATE INDEX runs_tenant_branch ON runs (tenant_id, branch, created_at DESC) WHERE kind = 'task';
