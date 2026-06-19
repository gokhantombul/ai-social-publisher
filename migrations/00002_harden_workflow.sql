-- +goose Up
-- +goose StatementBegin
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS fk_post_jobs_selected_variant;

UPDATE social_accounts
SET variant_count = LEAST(10, GREATEST(1, variant_count)),
    notify_threshold = LEAST(100, GREATEST(0, notify_threshold));
ALTER TABLE social_accounts
    ADD CONSTRAINT chk_social_accounts_variant_count CHECK (variant_count BETWEEN 1 AND 10),
    ADD CONSTRAINT chk_social_accounts_notify_threshold CHECK (notify_threshold BETWEEN 0 AND 100);
ALTER TABLE social_accounts DROP COLUMN auto_publish;

UPDATE social_accounts duplicate
SET is_active = FALSE, updated_at = now()
WHERE duplicate.is_active = TRUE
  AND EXISTS (
      SELECT 1 FROM social_accounts keeper
      WHERE keeper.category = duplicate.category
        AND keeper.is_active = TRUE
        AND keeper.id < duplicate.id
  );

CREATE UNIQUE INDEX uq_social_accounts_active_category
    ON social_accounts(category) WHERE is_active = TRUE;

UPDATE news_scores SET
    importance_score = LEAST(100, GREATEST(0, importance_score)),
    virality_score = LEAST(100, GREATEST(0, virality_score)),
    account_fit = CASE WHEN account_fit IN ('technology', 'cinema', 'news', 'economy', 'skip') THEN account_fit ELSE 'skip' END,
    risk_level = CASE WHEN risk_level IN ('low', 'medium', 'high') THEN risk_level ELSE 'low' END;
ALTER TABLE news_scores
    ADD CONSTRAINT chk_news_scores_importance CHECK (importance_score BETWEEN 0 AND 100),
    ADD CONSTRAINT chk_news_scores_virality CHECK (virality_score BETWEEN 0 AND 100),
    ADD CONSTRAINT chk_news_scores_account_fit CHECK (account_fit IN ('technology', 'cinema', 'news', 'economy', 'skip')),
    ADD CONSTRAINT chk_news_scores_risk CHECK (risk_level IN ('low', 'medium', 'high'));

DELETE FROM news_scores older
USING news_scores newer
WHERE older.news_candidate_id = newer.news_candidate_id AND older.id < newer.id;
CREATE UNIQUE INDEX uq_news_scores_candidate ON news_scores(news_candidate_id);

UPDATE news_candidates SET
    external_news_id = CASE
        WHEN btrim(external_news_id) = '' THEN 'legacy-empty-' || id::text
        WHEN length(external_news_id) > 500 THEN left(external_news_id, 467) || '-' || md5(external_news_id)
        ELSE external_news_id
    END,
    title = CASE WHEN btrim(title) = '' THEN 'Başlıksız haber' ELSE left(title, 500) END,
    summary = left(summary, 4000),
    source = left(source, 200),
    source_url = left(source_url, 2000);
ALTER TABLE news_candidates
    ADD CONSTRAINT chk_news_candidates_external_id CHECK (length(external_news_id) BETWEEN 1 AND 500),
    ADD CONSTRAINT chk_news_candidates_title CHECK (length(title) BETWEEN 1 AND 500),
    ADD CONSTRAINT chk_news_candidates_summary CHECK (length(summary) <= 4000),
    ADD CONSTRAINT chk_news_candidates_source CHECK (length(source) <= 200),
    ADD CONSTRAINT chk_news_candidates_source_url CHECK (length(source_url) <= 2000);

UPDATE post_jobs SET
    status = CASE WHEN status IN (
        'NEW', 'WAITING_AI', 'SCORED', 'WAITING_FIRST_APPROVAL',
        'GENERATING_VARIANTS', 'WAITING_VARIANT_APPROVAL', 'APPROVED',
        'PUBLISHING', 'PUBLISHED', 'SKIPPED', 'FAILED'
    ) THEN status ELSE 'FAILED' END,
    requested_variant_count = LEAST(10, GREATEST(0, requested_variant_count));
ALTER TABLE post_jobs
    ADD COLUMN ai_retry_count INT NOT NULL DEFAULT 0 CHECK (ai_retry_count >= 0),
    ADD COLUMN next_ai_retry_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD CONSTRAINT chk_post_jobs_status CHECK (status IN (
        'NEW', 'SCORING_QUEUED', 'SCORING', 'WAITING_AI', 'SCORED',
        'WAITING_FIRST_APPROVAL', 'VARIANTS_QUEUED', 'GENERATING_VARIANTS',
        'WAITING_VARIANT_APPROVAL', 'APPROVED', 'PUBLISHING', 'PUBLISHED',
        'SKIPPED', 'FAILED'
    )),
    ADD CONSTRAINT chk_post_jobs_variant_count CHECK (requested_variant_count BETWEEN 0 AND 10);

UPDATE post_jobs job SET selected_variant_id = NULL
FROM post_variants variant
WHERE job.selected_variant_id = variant.id
  AND (variant.post_job_id <> job.id OR length(btrim(variant.caption)) = 0);
DELETE FROM post_variants WHERE length(btrim(caption)) = 0;
UPDATE post_variants SET caption = left(caption, 2200);
WITH ranked AS (
    SELECT id, row_number() OVER (PARTITION BY post_job_id ORDER BY variant_no, id) AS new_no
    FROM post_variants
)
UPDATE post_variants variant SET variant_no = ranked.new_no
FROM ranked WHERE ranked.id = variant.id;
ALTER TABLE post_variants
    ADD CONSTRAINT uq_post_variants_job_number UNIQUE (post_job_id, variant_no),
    ADD CONSTRAINT uq_post_variants_id_job UNIQUE (id, post_job_id),
    ADD CONSTRAINT chk_post_variants_number CHECK (variant_no > 0),
    ADD CONSTRAINT chk_post_variants_caption CHECK (length(caption) BETWEEN 1 AND 2200);

ALTER TABLE post_jobs
    ADD CONSTRAINT fk_post_jobs_selected_variant_same_job
    FOREIGN KEY (selected_variant_id, id)
    REFERENCES post_variants(id, post_job_id)
    ON DELETE SET NULL (selected_variant_id);

CREATE TABLE notification_outbox (
    id              BIGSERIAL PRIMARY KEY,
    dedupe_key      TEXT        NOT NULL UNIQUE,
    payload         JSONB       NOT NULL,
    attempts        INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    locked_until    TIMESTAMPTZ,
    sent_at         TIMESTAMPTZ,
    dead_at         TIMESTAMPTZ,
    last_error      TEXT        NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_notification_outbox_due
    ON notification_outbox(next_attempt_at)
    WHERE sent_at IS NULL AND dead_at IS NULL;
CREATE INDEX idx_post_jobs_status_updated ON post_jobs(status, updated_at);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_post_jobs_status_updated;
DROP TABLE IF EXISTS notification_outbox;
DROP INDEX IF EXISTS uq_news_scores_candidate;
ALTER TABLE post_jobs DROP CONSTRAINT IF EXISTS fk_post_jobs_selected_variant_same_job;
ALTER TABLE post_variants
    DROP CONSTRAINT IF EXISTS chk_post_variants_caption,
    DROP CONSTRAINT IF EXISTS chk_post_variants_number,
    DROP CONSTRAINT IF EXISTS uq_post_variants_id_job,
    DROP CONSTRAINT IF EXISTS uq_post_variants_job_number;
ALTER TABLE post_jobs
    DROP COLUMN IF EXISTS next_ai_retry_at,
    DROP COLUMN IF EXISTS ai_retry_count,
    DROP CONSTRAINT IF EXISTS chk_post_jobs_variant_count,
    DROP CONSTRAINT IF EXISTS chk_post_jobs_status;
ALTER TABLE news_scores
    DROP CONSTRAINT IF EXISTS chk_news_scores_risk,
    DROP CONSTRAINT IF EXISTS chk_news_scores_account_fit,
    DROP CONSTRAINT IF EXISTS chk_news_scores_virality,
    DROP CONSTRAINT IF EXISTS chk_news_scores_importance;
ALTER TABLE news_candidates
    DROP CONSTRAINT IF EXISTS chk_news_candidates_source_url,
    DROP CONSTRAINT IF EXISTS chk_news_candidates_source,
    DROP CONSTRAINT IF EXISTS chk_news_candidates_summary,
    DROP CONSTRAINT IF EXISTS chk_news_candidates_title,
    DROP CONSTRAINT IF EXISTS chk_news_candidates_external_id;
DROP INDEX IF EXISTS uq_social_accounts_active_category;
ALTER TABLE social_accounts
    DROP CONSTRAINT IF EXISTS chk_social_accounts_notify_threshold,
    DROP CONSTRAINT IF EXISTS chk_social_accounts_variant_count;
ALTER TABLE social_accounts ADD COLUMN auto_publish BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE post_jobs
    ADD CONSTRAINT fk_post_jobs_selected_variant
    FOREIGN KEY (selected_variant_id) REFERENCES post_variants(id) ON DELETE SET NULL;
-- +goose StatementEnd
