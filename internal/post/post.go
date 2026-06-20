// Package post manages post_jobs, post_variants and publish_logs persistence,
// including the controlled status state machine.
package post

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrNotFound is returned when a job/variant lookup yields no row.
	ErrNotFound = errors.New("post job not found")
	// ErrInvalidTransition is returned when a status change is not allowed.
	ErrInvalidTransition = errors.New("invalid status transition")
)

// Job mirrors the post_jobs table.
type Job struct {
	ID                    int64         `json:"id"`
	NewsCandidateID       int64         `json:"newsCandidateId"`
	SocialAccountID       int64         `json:"socialAccountId"`
	Status                Status        `json:"status"`
	RequestedVariantCount int           `json:"requestedVariantCount"`
	SelectedVariantID     sql.NullInt64 `json:"-"`
	InstagramMediaID      string        `json:"instagramMediaId"`
	AIProvider            string        `json:"aiProvider"`
	AIModel               string        `json:"aiModel"`
	AIError               string        `json:"aiError"`
	ErrorMessage          string        `json:"errorMessage"`
	AIRetryCount          int           `json:"aiRetryCount"`
	NextAIRetryAt         time.Time     `json:"nextAiRetryAt"`
	CreatedAt             time.Time     `json:"createdAt"`
	UpdatedAt             time.Time     `json:"updatedAt"`
}

// Variant mirrors the post_variants table.
type Variant struct {
	ID        int64     `json:"id"`
	PostJobID int64     `json:"postJobId"`
	VariantNo int       `json:"variantNo"`
	Style     string    `json:"style"`
	Caption   string    `json:"caption"`
	ImageURL  string    `json:"imageUrl"`
	CreatedAt time.Time `json:"createdAt"`
}

// Repository provides persistence for jobs, variants and publish logs.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// GetOrCreate returns the existing job for a (candidate, account) pair or creates
// a new one in StatusNew. The second return reports whether it was created.
func (r *Repository) GetOrCreate(ctx context.Context, newsCandidateID, socialAccountID int64) (*Job, bool, error) {
	const insert = `
INSERT INTO post_jobs (news_candidate_id, social_account_id, status)
VALUES ($1, $2, $3)
ON CONFLICT (news_candidate_id, social_account_id) DO NOTHING
RETURNING id, news_candidate_id, social_account_id, status, requested_variant_count,
          selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
          error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at`

	job, err := scanJob(r.db.QueryRowContext(ctx, insert, newsCandidateID, socialAccountID, StatusNew))
	if errors.Is(err, sql.ErrNoRows) {
		existing, gerr := r.getBy(ctx, "news_candidate_id = $1 AND social_account_id = $2", newsCandidateID, socialAccountID)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &job, true, nil
}

func (r *Repository) GetByID(ctx context.Context, id int64) (*Job, error) {
	return r.getBy(ctx, "id = $1", id)
}

func (r *Repository) getBy(ctx context.Context, where string, args ...any) (*Job, error) {
	q := `SELECT id, news_candidate_id, social_account_id, status, requested_variant_count,
       selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
	       error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at
FROM post_jobs WHERE ` + where + ` LIMIT 1`
	job, err := scanJob(r.db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (r *Repository) List(ctx context.Context, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT id, news_candidate_id, social_account_id, status, requested_variant_count,
       selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
	       error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at
FROM post_jobs ORDER BY id DESC LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListByStatus returns jobs in a given status (used by the scheduler).
func (r *Repository) ListByStatus(ctx context.Context, status Status, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT id, news_candidate_id, social_account_id, status, requested_variant_count,
       selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
	       error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at
	FROM post_jobs WHERE status = $1 AND next_ai_retry_at <= now() ORDER BY updated_at ASC LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (r *Repository) ListRecentByStatus(ctx context.Context, status Status, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT id, news_candidate_id, social_account_id, status, requested_variant_count,
       selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
       error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at
FROM post_jobs WHERE status = $1 ORDER BY updated_at DESC LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateStatus moves a job to a new status after validating the transition. It
// re-reads the current status inside a transaction to avoid races.
func (r *Repository) UpdateStatus(ctx context.Context, id int64, to Status) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var current Status
	if err := tx.QueryRowContext(ctx, `SELECT status FROM post_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if !CanTransition(current, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, to)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE post_jobs SET status = $1, updated_at = now() WHERE id = $2`, to, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ClaimStatus atomically moves a job from the exact expected state to the next
// state. The bool is false when another worker already claimed or completed it.
// External side effects must only run after a successful claim.
func (r *Repository) ClaimStatus(ctx context.Context, id int64, from, to Status) (bool, error) {
	if !CanTransition(from, to) {
		return false, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	result, err := r.db.ExecContext(ctx,
		`UPDATE post_jobs SET status = $1, updated_at = now() WHERE id = $2 AND status = $3`,
		to, id, from)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ScoreUpdate carries AI scoring metadata applied alongside a status change.
type ScoreUpdate struct {
	Status     Status
	AIProvider string
	AIModel    string
	AIError    string
}

// ApplyScored sets AI metadata and transitions to the given status atomically.
func (r *Repository) ApplyScored(ctx context.Context, id int64, u ScoreUpdate) error {
	return r.transition(ctx, id, u.Status, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET ai_provider = $1, ai_model = $2, ai_error = $3, ai_retry_count = 0, next_ai_retry_at = now() WHERE id = $4`,
			u.AIProvider, u.AIModel, u.AIError, id)
		return err
	})
}

func (r *Repository) CompleteAIStage(ctx context.Context, id int64, to Status) error {
	return r.transition(ctx, id, to, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE post_jobs SET ai_error = '', ai_retry_count = 0, next_ai_retry_at = now() WHERE id = $1`, id)
		return err
	})
}

// ParkForAIRetry applies bounded exponential backoff. After maxRetries the job
// becomes FAILED instead of retrying forever.
func (r *Repository) ParkForAIRetry(ctx context.Context, id int64, aiError string) error {
	const maxRetries = 8
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var current Status
	var retries int
	if err := tx.QueryRowContext(ctx, `SELECT status, ai_retry_count FROM post_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&current, &retries); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !CanTransition(current, StatusWaitingAI) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, StatusWaitingAI)
	}
	retries++
	if retries >= maxRetries {
		_, err = tx.ExecContext(ctx, `UPDATE post_jobs
SET status = $1, ai_error = $2, error_message = $3, ai_retry_count = $4, updated_at = now()
WHERE id = $5`, StatusFailed, aiError, "AI retries exhausted: "+aiError, retries, id)
	} else {
		delay := time.Minute << min(retries-1, 4)
		_, err = tx.ExecContext(ctx, `UPDATE post_jobs
SET status = $1, ai_error = $2, ai_retry_count = $3, next_ai_retry_at = $4, updated_at = now()
WHERE id = $5`, StatusWaitingAI, aiError, retries, time.Now().Add(delay), id)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// QueueVariants records how many variants were requested and queues generation.
func (r *Repository) QueueVariants(ctx context.Context, id int64, count int) error {
	return r.transition(ctx, id, StatusVariantsQueued, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET requested_variant_count = $1, selected_variant_id = NULL WHERE id = $2`, count, id)
		return err
	})
}

// SelectVariant records the chosen variant and moves to READY_TO_PUBLISH.
func (r *Repository) SelectVariant(ctx context.Context, id, variantID int64) error {
	return r.transition(ctx, id, StatusReadyToPublish, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET selected_variant_id = $1
			 WHERE id = $2 AND EXISTS (
				 SELECT 1 FROM post_variants WHERE id = $1 AND post_job_id = $2
			 )`, variantID, id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("%w: variant does not belong to job", ErrNotFound)
		}
		return nil
	})
}

// ReselectVariant changes the selected variant while a job is still under
// operator review. It deliberately does not accept later publish states.
func (r *Repository) ReselectVariant(ctx context.Context, id, variantID int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var current Status
	if err := tx.QueryRowContext(ctx, `SELECT status FROM post_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if current != StatusReadyToPublish {
		return fmt.Errorf("%w: cannot reselect variant in %s", ErrInvalidTransition, current)
	}
	result, err := tx.ExecContext(ctx, `UPDATE post_jobs SET selected_variant_id = $1, updated_at = now()
WHERE id = $2 AND EXISTS (SELECT 1 FROM post_variants WHERE id = $1 AND post_job_id = $2)`, variantID, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("%w: variant does not belong to job", ErrNotFound)
	}
	return tx.Commit()
}

// UpdateVariantCaption edits a variant only while an operator can still review
// it. The preview URL is cleared in the same transaction so stale artwork is
// never presented after a caption change. The bool reports whether the edited
// variant is currently selected and therefore needs a new preview.
func (r *Repository) UpdateVariantCaption(ctx context.Context, jobID, variantID int64, caption string) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback() //nolint:errcheck

	var current Status
	var selected sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT status, selected_variant_id FROM post_jobs WHERE id = $1 FOR UPDATE`, jobID).Scan(&current, &selected); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if current != StatusWaitingVariantApproval && current != StatusReadyToPublish {
		return false, fmt.Errorf("%w: caption cannot be edited in %s", ErrInvalidTransition, current)
	}
	result, err := tx.ExecContext(ctx, `UPDATE post_variants SET caption = $1, image_url = '' WHERE id = $2 AND post_job_id = $3`, caption, variantID, jobID)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil {
		return false, err
	} else if n != 1 {
		return false, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return selected.Valid && selected.Int64 == variantID, nil
}

// ListStaleProcessing returns jobs left in an in-progress state beyond cutoff.
func (r *Repository) ListStaleProcessing(ctx context.Context, cutoff time.Time, limit int) ([]Job, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT id, news_candidate_id, social_account_id, status, requested_variant_count,
       selected_variant_id, instagram_media_id, ai_provider, ai_model, ai_error,
	       error_message, ai_retry_count, next_ai_retry_at, created_at, updated_at
FROM post_jobs
WHERE status IN ($1, $2, $3) AND updated_at < $4
ORDER BY updated_at ASC LIMIT $5`
	rows, err := r.db.QueryContext(ctx, q, StatusScoring, StatusGeneratingVariants, StatusPublishing, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// MarkPublished records the instagram media id and moves to PUBLISHED.
func (r *Repository) MarkPublished(ctx context.Context, id int64, mediaID string) error {
	return r.transition(ctx, id, StatusPublished, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET instagram_media_id = $1, error_message = '' WHERE id = $2`, mediaID, id)
		return err
	})
}

// MarkFailed records an error message and moves to FAILED.
func (r *Repository) MarkFailed(ctx context.Context, id int64, msg string) error {
	return r.transition(ctx, id, StatusFailed, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE post_jobs SET error_message = $1 WHERE id = $2`, msg, id)
		return err
	})
}

// transition runs a guarded status change plus an extra mutation in one tx.
func (r *Repository) transition(ctx context.Context, id int64, to Status, extra func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var current Status
	if err := tx.QueryRowContext(ctx, `SELECT status FROM post_jobs WHERE id = $1 FOR UPDATE`, id).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !CanTransition(current, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, current, to)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE post_jobs SET status = $1, updated_at = now() WHERE id = $2`, to, id); err != nil {
		return err
	}
	if extra != nil {
		if err := extra(tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- variants ----

// ReplaceVariants deletes existing variants for a job and inserts the new set.
func (r *Repository) ReplaceVariants(ctx context.Context, jobID int64, variants []Variant) ([]Variant, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `UPDATE post_jobs SET selected_variant_id = NULL WHERE id = $1`, jobID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM post_variants WHERE post_job_id = $1`, jobID); err != nil {
		return nil, err
	}

	out := make([]Variant, 0, len(variants))
	for _, v := range variants {
		var id int64
		var createdAt time.Time
		err := tx.QueryRowContext(ctx,
			`INSERT INTO post_variants (post_job_id, variant_no, style, caption, image_url)
VALUES ($1, $2, $3, $4, $5) RETURNING id, created_at`,
			jobID, v.VariantNo, v.Style, v.Caption, v.ImageURL).Scan(&id, &createdAt)
		if err != nil {
			return nil, err
		}
		v.ID = id
		v.PostJobID = jobID
		v.CreatedAt = createdAt
		out = append(out, v)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

func (r *Repository) ListVariants(ctx context.Context, jobID int64) ([]Variant, error) {
	const q = `SELECT id, post_job_id, variant_no, style, caption, image_url, created_at
FROM post_variants WHERE post_job_id = $1 ORDER BY variant_no`
	rows, err := r.db.QueryContext(ctx, q, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Variant
	for rows.Next() {
		var v Variant
		if err := rows.Scan(&v.ID, &v.PostJobID, &v.VariantNo, &v.Style, &v.Caption, &v.ImageURL, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (r *Repository) GetVariantByID(ctx context.Context, id int64) (*Variant, error) {
	const q = `SELECT id, post_job_id, variant_no, style, caption, image_url, created_at
FROM post_variants WHERE id = $1`
	var v Variant
	err := r.db.QueryRowContext(ctx, q, id).Scan(
		&v.ID, &v.PostJobID, &v.VariantNo, &v.Style, &v.Caption, &v.ImageURL, &v.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// SetVariantImageURL stores the rendered image URL for a variant.
func (r *Repository) SetVariantImageURL(ctx context.Context, variantID int64, url string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE post_variants SET image_url = $1 WHERE id = $2`, url, variantID)
	return err
}

// SetVariantImageURLForCaption stores a preview only if the caption used to
// render it is still current. This prevents a slower concurrent render from
// restoring stale artwork after an operator edit.
func (r *Repository) SetVariantImageURLForCaption(ctx context.Context, variantID int64, caption, url string) (bool, error) {
	result, err := r.db.ExecContext(ctx, `UPDATE post_variants SET image_url = $1 WHERE id = $2 AND caption = $3`, url, variantID, caption)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ---- publish logs ----

// PublishLog mirrors the publish_logs table.
type PublishLog struct {
	PostJobID       int64
	Platform        string
	RequestPayload  json.RawMessage
	ResponsePayload json.RawMessage
	Success         bool
	ErrorMessage    string
}

func (r *Repository) InsertPublishLog(ctx context.Context, l PublishLog) error {
	if l.Platform == "" {
		l.Platform = "instagram"
	}
	const q = `
INSERT INTO publish_logs (post_job_id, platform, request_payload, response_payload, success, error_message)
VALUES ($1, $2, $3, $4, $5, $6)`
	_, err := r.db.ExecContext(ctx, q,
		l.PostJobID, l.Platform, nullableJSON(l.RequestPayload), nullableJSON(l.ResponsePayload), l.Success, l.ErrorMessage)
	return err
}

// SuccessfulPublishMediaID supports crash reconciliation between the external
// publish succeeding and post_jobs being marked PUBLISHED.
func (r *Repository) SuccessfulPublishMediaID(ctx context.Context, jobID int64) (string, bool, error) {
	var payload []byte
	err := r.db.QueryRowContext(ctx, `
SELECT response_payload FROM publish_logs
WHERE post_job_id = $1 AND success = TRUE AND response_payload IS NOT NULL
ORDER BY id DESC LIMIT 1`, jobID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var response struct {
		ID      string `json:"id"`
		MediaID string `json:"media_id"`
	}
	if err := json.Unmarshal(payload, &response); err != nil {
		return "", false, err
	}
	if response.ID != "" {
		return response.ID, true, nil
	}
	if response.MediaID != "" {
		return response.MediaID, true, nil
	}
	return "", false, nil
}

// ---- analytics ----

// StatusCounts returns the number of jobs per status.
func (r *Repository) StatusCounts(ctx context.Context) (map[string]int, error) {
	const q = `SELECT status, COUNT(*) FROM post_jobs GROUP BY status`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		out[status] = count
	}
	return out, rows.Err()
}

func nullableJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func scanJob(s interface{ Scan(...any) error }) (Job, error) {
	var j Job
	err := s.Scan(
		&j.ID, &j.NewsCandidateID, &j.SocialAccountID, &j.Status, &j.RequestedVariantCount,
		&j.SelectedVariantID, &j.InstagramMediaID, &j.AIProvider, &j.AIModel, &j.AIError,
		&j.ErrorMessage, &j.AIRetryCount, &j.NextAIRetryAt, &j.CreatedAt, &j.UpdatedAt,
	)
	return j, err
}
