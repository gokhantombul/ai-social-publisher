-- +goose Up
-- +goose StatementBegin
CREATE TABLE social_accounts (
    id                BIGSERIAL PRIMARY KEY,
    code              TEXT        NOT NULL UNIQUE,
    name              TEXT        NOT NULL,
    category          TEXT        NOT NULL,
    instagram_user_id TEXT        NOT NULL DEFAULT '',
    variant_count     INT         NOT NULL DEFAULT 3,
    notify_threshold  INT         NOT NULL DEFAULT 80,
    auto_publish      BOOLEAN     NOT NULL DEFAULT FALSE,
    is_active         BOOLEAN     NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE news_candidates (
    id               BIGSERIAL PRIMARY KEY,
    external_news_id TEXT        NOT NULL UNIQUE,
    title            TEXT        NOT NULL,
    summary          TEXT        NOT NULL DEFAULT '',
    source           TEXT        NOT NULL DEFAULT '',
    source_url       TEXT        NOT NULL DEFAULT '',
    category         TEXT        NOT NULL,
    published_at     TIMESTAMPTZ,
    raw_payload      JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE news_scores (
    id                 BIGSERIAL PRIMARY KEY,
    news_candidate_id  BIGINT      NOT NULL REFERENCES news_candidates(id) ON DELETE CASCADE,
    importance_score   INT         NOT NULL DEFAULT 0,
    virality_score     INT         NOT NULL DEFAULT 0,
    account_fit        TEXT        NOT NULL DEFAULT 'skip',
    should_notify      BOOLEAN     NOT NULL DEFAULT FALSE,
    risk_level         TEXT        NOT NULL DEFAULT 'low',
    reason             TEXT        NOT NULL DEFAULT '',
    ai_provider        TEXT        NOT NULL DEFAULT '',
    ai_model           TEXT        NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE post_jobs (
    id                      BIGSERIAL PRIMARY KEY,
    news_candidate_id       BIGINT      NOT NULL REFERENCES news_candidates(id) ON DELETE CASCADE,
    social_account_id       BIGINT      NOT NULL REFERENCES social_accounts(id) ON DELETE RESTRICT,
    status                  TEXT        NOT NULL DEFAULT 'NEW',
    requested_variant_count INT         NOT NULL DEFAULT 0,
    selected_variant_id     BIGINT,
    instagram_media_id      TEXT        NOT NULL DEFAULT '',
    ai_provider             TEXT        NOT NULL DEFAULT '',
    ai_model                TEXT        NOT NULL DEFAULT '',
    ai_error                TEXT        NOT NULL DEFAULT '',
    error_message           TEXT        NOT NULL DEFAULT '',
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (news_candidate_id, social_account_id)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE post_variants (
    id          BIGSERIAL PRIMARY KEY,
    post_job_id BIGINT      NOT NULL REFERENCES post_jobs(id) ON DELETE CASCADE,
    variant_no  INT         NOT NULL,
    style       TEXT        NOT NULL DEFAULT '',
    caption     TEXT        NOT NULL DEFAULT '',
    image_url   TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
-- post_jobs.selected_variant_id references a post_variants row; added after both
-- tables exist to avoid a circular dependency.
ALTER TABLE post_jobs
    ADD CONSTRAINT fk_post_jobs_selected_variant
    FOREIGN KEY (selected_variant_id) REFERENCES post_variants(id) ON DELETE SET NULL;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE publish_logs (
    id               BIGSERIAL PRIMARY KEY,
    post_job_id      BIGINT      NOT NULL REFERENCES post_jobs(id) ON DELETE CASCADE,
    platform         TEXT        NOT NULL DEFAULT 'instagram',
    request_payload  JSONB,
    response_payload JSONB,
    success          BOOLEAN     NOT NULL DEFAULT FALSE,
    error_message    TEXT        NOT NULL DEFAULT '',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE INDEX idx_news_scores_candidate     ON news_scores(news_candidate_id);
CREATE INDEX idx_post_jobs_status          ON post_jobs(status);
CREATE INDEX idx_post_jobs_candidate       ON post_jobs(news_candidate_id);
CREATE INDEX idx_post_variants_job         ON post_variants(post_job_id);
CREATE INDEX idx_publish_logs_job          ON publish_logs(post_job_id);
CREATE INDEX idx_news_candidates_category  ON news_candidates(category);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS publish_logs;
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS fk_post_jobs_selected_variant;
DROP TABLE IF EXISTS post_variants;
DROP TABLE IF EXISTS post_jobs;
DROP TABLE IF EXISTS news_scores;
DROP TABLE IF EXISTS news_candidates;
DROP TABLE IF EXISTS social_accounts;
-- +goose StatementEnd
