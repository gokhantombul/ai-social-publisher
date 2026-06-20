-- +goose Up
-- +goose StatementBegin
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS chk_post_jobs_status;
ALTER TABLE post_jobs
    ADD CONSTRAINT chk_post_jobs_status CHECK (status IN (
        'NEW', 'SCORING_QUEUED', 'SCORING', 'WAITING_AI', 'SCORED',
        'WAITING_FIRST_APPROVAL', 'VARIANTS_QUEUED', 'GENERATING_VARIANTS',
        'WAITING_VARIANT_APPROVAL', 'READY_TO_PUBLISH', 'APPROVED',
        'PUBLISHING', 'PUBLISHED', 'SKIPPED', 'FAILED'
    ));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
UPDATE post_jobs SET status = 'WAITING_VARIANT_APPROVAL' WHERE status = 'READY_TO_PUBLISH';
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS chk_post_jobs_status;
ALTER TABLE post_jobs
    ADD CONSTRAINT chk_post_jobs_status CHECK (status IN (
        'NEW', 'SCORING_QUEUED', 'SCORING', 'WAITING_AI', 'SCORED',
        'WAITING_FIRST_APPROVAL', 'VARIANTS_QUEUED', 'GENERATING_VARIANTS',
        'WAITING_VARIANT_APPROVAL', 'APPROVED', 'PUBLISHING', 'PUBLISHED',
        'SKIPPED', 'FAILED'
    ));
-- +goose StatementEnd
