// Package news handles news candidate persistence, scoring records and the
// external news-service client.
package news

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is returned when a candidate lookup yields no row.
var ErrNotFound = errors.New("news candidate not found")

// Candidate mirrors the news_candidates table.
type Candidate struct {
	ID             int64           `json:"id"`
	ExternalNewsID string          `json:"externalNewsId"`
	Title          string          `json:"title"`
	Summary        string          `json:"summary"`
	Source         string          `json:"source"`
	SourceURL      string          `json:"sourceUrl"`
	Category       string          `json:"category"`
	PublishedAt    *time.Time      `json:"publishedAt,omitempty"`
	RawPayload     json.RawMessage `json:"-"`
	CreatedAt      time.Time       `json:"createdAt"`
}

// Score mirrors the news_scores table.
type Score struct {
	ID              int64     `json:"id"`
	NewsCandidateID int64     `json:"newsCandidateId"`
	ImportanceScore int       `json:"importanceScore"`
	ViralityScore   int       `json:"viralityScore"`
	AccountFit      string    `json:"accountFit"`
	ShouldNotify    bool      `json:"shouldNotify"`
	RiskLevel       string    `json:"riskLevel"`
	Reason          string    `json:"reason"`
	AIProvider      string    `json:"aiProvider"`
	AIModel         string    `json:"aiModel"`
	CreatedAt       time.Time `json:"createdAt"`
}

// Repository provides persistence for candidates and scores.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// Upsert inserts a candidate, ignoring duplicates by external_news_id. The
// returned bool reports whether a new row was created (true) or it already
// existed (false). This is the duplicate-control mechanism.
func (r *Repository) Upsert(ctx context.Context, c Candidate) (*Candidate, bool, error) {
	const q = `
INSERT INTO news_candidates (external_news_id, title, summary, source, source_url, category, published_at, raw_payload)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (external_news_id) DO NOTHING
RETURNING id, external_news_id, title, summary, source, source_url, category, published_at, created_at`

	row := r.db.QueryRowContext(ctx, q,
		c.ExternalNewsID, c.Title, c.Summary, c.Source, c.SourceURL, c.Category, c.PublishedAt, c.RawPayload)

	created, err := scanCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		// Conflict: row already exists. Fetch the existing one.
		existing, gerr := r.GetByExternalID(ctx, c.ExternalNewsID)
		if gerr != nil {
			return nil, false, gerr
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &created, true, nil
}

func (r *Repository) GetByExternalID(ctx context.Context, externalID string) (*Candidate, error) {
	const q = `SELECT id, external_news_id, title, summary, source, source_url, category, published_at, created_at
FROM news_candidates WHERE external_news_id = $1`
	c, err := scanCandidate(r.db.QueryRowContext(ctx, q, externalID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repository) GetByID(ctx context.Context, id int64) (*Candidate, error) {
	const q = `SELECT id, external_news_id, title, summary, source, source_url, category, published_at, created_at
FROM news_candidates WHERE id = $1`
	c, err := scanCandidate(r.db.QueryRowContext(ctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repository) List(ctx context.Context, limit int) ([]Candidate, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	const q = `SELECT id, external_news_id, title, summary, source, source_url, category, published_at, created_at
FROM news_candidates ORDER BY id DESC LIMIT $1`
	rows, err := r.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Candidate
	for rows.Next() {
		c, err := scanCandidate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveScore inserts a scoring record.
func (r *Repository) SaveScore(ctx context.Context, s Score) (*Score, error) {
	const q = `
INSERT INTO news_scores (news_candidate_id, importance_score, virality_score, account_fit, should_notify, risk_level, reason, ai_provider, ai_model)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, created_at`
	err := r.db.QueryRowContext(ctx, q,
		s.NewsCandidateID, s.ImportanceScore, s.ViralityScore, s.AccountFit,
		s.ShouldNotify, s.RiskLevel, s.Reason, s.AIProvider, s.AIModel,
	).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanCandidate(s scanner) (Candidate, error) {
	var c Candidate
	err := s.Scan(
		&c.ID, &c.ExternalNewsID, &c.Title, &c.Summary, &c.Source,
		&c.SourceURL, &c.Category, &c.PublishedAt, &c.CreatedAt,
	)
	return c, err
}
