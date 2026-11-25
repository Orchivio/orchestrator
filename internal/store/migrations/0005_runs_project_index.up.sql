CREATE INDEX runs_project_created_at ON runs (tenant_id, project_id, created_at DESC, id DESC);
