-- The agent's ask_human question is a history row of its own, so the
-- conversation keeps question-before-answer order after the run resumes.
ALTER TABLE inputs ADD COLUMN from_agent boolean NOT NULL DEFAULT false;
