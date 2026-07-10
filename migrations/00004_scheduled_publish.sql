-- +goose Up
-- +goose StatementBegin
-- Deferred publishing: a reviewed job may be scheduled to publish at a future
-- time instead of immediately. scheduled_publish_at carries that target time and
-- is only meaningful while the job sits in the SCHEDULED state.
ALTER TABLE post_jobs ADD COLUMN scheduled_publish_at TIMESTAMPTZ;

ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS chk_post_jobs_status;
ALTER TABLE post_jobs
    ADD CONSTRAINT chk_post_jobs_status CHECK (status IN (
        'NEW', 'SCORING_QUEUED', 'SCORING', 'WAITING_AI', 'SCORED',
        'WAITING_FIRST_APPROVAL', 'VARIANTS_QUEUED', 'GENERATING_VARIANTS',
        'WAITING_VARIANT_APPROVAL', 'READY_TO_PUBLISH', 'SCHEDULED', 'APPROVED',
        'PUBLISHING', 'PUBLISHED', 'SKIPPED', 'FAILED'
    ));

-- A SCHEDULED job must always carry its target time. Other states may retain the
-- value for audit after promotion but are not required to have one. CHECK
-- constraints cannot be deferred, so writers set status and time in one UPDATE.
ALTER TABLE post_jobs
    ADD CONSTRAINT chk_post_jobs_scheduled_at CHECK (
        status <> 'SCHEDULED' OR scheduled_publish_at IS NOT NULL
    );

-- Supports the due-scheduled scan cheaply.
CREATE INDEX idx_post_jobs_scheduled
    ON post_jobs(scheduled_publish_at)
    WHERE status = 'SCHEDULED';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE post_jobs SET status = 'READY_TO_PUBLISH', scheduled_publish_at = NULL WHERE status = 'SCHEDULED';
DROP INDEX IF EXISTS idx_post_jobs_scheduled;
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS chk_post_jobs_scheduled_at;
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS chk_post_jobs_status;
ALTER TABLE post_jobs
    ADD CONSTRAINT chk_post_jobs_status CHECK (status IN (
        'NEW', 'SCORING_QUEUED', 'SCORING', 'WAITING_AI', 'SCORED',
        'WAITING_FIRST_APPROVAL', 'VARIANTS_QUEUED', 'GENERATING_VARIANTS',
        'WAITING_VARIANT_APPROVAL', 'READY_TO_PUBLISH', 'APPROVED',
        'PUBLISHING', 'PUBLISHED', 'SKIPPED', 'FAILED'
    ));
ALTER TABLE post_jobs DROP COLUMN IF EXISTS scheduled_publish_at;
-- +goose StatementEnd
