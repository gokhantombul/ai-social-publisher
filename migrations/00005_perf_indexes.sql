-- +goose Up
-- +goose StatementBegin
-- The admin console lists jobs by recency without a status filter (post queue
-- default view and dashboard "recent jobs"); support that ordering directly
-- instead of sorting the whole table.
CREATE INDEX idx_post_jobs_updated ON post_jobs(updated_at DESC, id DESC);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_post_jobs_updated;
-- +goose StatementEnd
