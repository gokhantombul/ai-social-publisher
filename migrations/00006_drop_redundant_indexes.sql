-- +goose Up
-- +goose StatementBegin
-- idx_post_jobs_status is a strict prefix of idx_post_jobs_status_updated
-- (status, updated_at), and idx_news_scores_candidate duplicates the unique
-- index uq_news_scores_candidate. Both only inflate write cost.
DROP INDEX IF EXISTS idx_post_jobs_status;
DROP INDEX IF EXISTS idx_news_scores_candidate;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE INDEX idx_post_jobs_status ON post_jobs(status);
CREATE INDEX idx_news_scores_candidate ON news_scores(news_candidate_id);
-- +goose StatementEnd
