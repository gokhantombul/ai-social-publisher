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
          error_message, created_at, updated_at`

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
       error_message, created_at, updated_at
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
       error_message, created_at, updated_at
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
       error_message, created_at, updated_at
FROM post_jobs WHERE status = $1 ORDER BY updated_at ASC LIMIT $2`
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
			`UPDATE post_jobs SET ai_provider = $1, ai_model = $2, ai_error = $3 WHERE id = $4`,
			u.AIProvider, u.AIModel, u.AIError, id)
		return err
	})
}

// SetRequestedVariantCount records how many variants were requested and moves to
// GENERATING_VARIANTS.
func (r *Repository) SetRequestedVariantCount(ctx context.Context, id int64, count int) error {
	return r.transition(ctx, id, StatusGeneratingVariants, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET requested_variant_count = $1 WHERE id = $2`, count, id)
		return err
	})
}

// SelectVariant records the chosen variant and moves to APPROVED.
func (r *Repository) SelectVariant(ctx context.Context, id, variantID int64) error {
	return r.transition(ctx, id, StatusApproved, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE post_jobs SET selected_variant_id = $1 WHERE id = $2`, variantID, id)
		return err
	})
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
		&j.ErrorMessage, &j.CreatedAt, &j.UpdatedAt,
	)
	return j, err
}
