package admin

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"ai-social-publisher/internal/post"
)

const pageSize = 25

// Repository contains read-only, cross-domain projections used by the console.
// Workflow writes continue to go through approval.Service.
type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

type JobView struct {
	ID                    int64
	NewsCandidateID       int64
	SocialAccountID       int64
	Status                post.Status
	RequestedVariantCount int
	SelectedVariantID     sql.NullInt64
	InstagramMediaID      string
	AIProvider            string
	AIModel               string
	AIError               string
	ErrorMessage          string
	AIRetryCount          int
	NextAIRetryAt         time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Title                 string
	Summary               string
	Source                string
	SourceURL             string
	Category              string
	PublishedAt           sql.NullTime
	AccountCode           string
	AccountName           string
	InstagramUserID       string
	VariantCount          int
	NotifyThreshold       int
	AccountActive         bool
	HasScore              bool
	ImportanceScore       int
	ViralityScore         int
	AccountFit            string
	ShouldNotify          bool
	RiskLevel             string
	ScoreReason           string
}

type JobFilters struct {
	Status   string
	Category string
	Account  string
	Query    string
	Page     int
}

type PageInfo struct {
	Page       int
	PageSize   int
	Total      int
	TotalPages int
	HasPrev    bool
	HasNext    bool
}

type JobPage struct {
	Items []JobView
	Page  PageInfo
}

const jobSelect = `
SELECT j.id, j.news_candidate_id, j.social_account_id, j.status, j.requested_variant_count,
       j.selected_variant_id, j.instagram_media_id, j.ai_provider, j.ai_model, j.ai_error,
       j.error_message, j.ai_retry_count, j.next_ai_retry_at, j.created_at, j.updated_at,
       c.title, c.summary, c.source, c.source_url, c.category, c.published_at,
       a.code, a.name, a.instagram_user_id, a.variant_count, a.notify_threshold, a.is_active,
       (s.id IS NOT NULL), COALESCE(s.importance_score, 0), COALESCE(s.virality_score, 0),
       COALESCE(s.account_fit, ''), COALESCE(s.should_notify, FALSE), COALESCE(s.risk_level, ''),
       COALESCE(s.reason, '')
FROM post_jobs j
JOIN news_candidates c ON c.id = j.news_candidate_id
JOIN social_accounts a ON a.id = j.social_account_id
LEFT JOIN news_scores s ON s.news_candidate_id = c.id`

func (r *Repository) GetJob(ctx context.Context, id int64) (*JobView, error) {
	row := r.db.QueryRowContext(ctx, jobSelect+` WHERE j.id = $1`, id)
	v, err := scanJobView(row)
	if err == sql.ErrNoRows {
		return nil, post.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *Repository) ListJobs(ctx context.Context, f JobFilters) (JobPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	where, args := jobWhere(f)
	var total int
	countQuery := `SELECT COUNT(*) FROM post_jobs j
JOIN news_candidates c ON c.id = j.news_candidate_id
JOIN social_accounts a ON a.id = j.social_account_id ` + where
	if err := r.db.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return JobPage{}, err
	}

	args = append(args, pageSize, (f.Page-1)*pageSize)
	query := jobSelect + ` ` + where + fmt.Sprintf(` ORDER BY j.updated_at DESC, j.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return JobPage{}, err
	}
	defer rows.Close()
	items := make([]JobView, 0, pageSize)
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return JobPage{}, err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return JobPage{}, err
	}
	return JobPage{Items: items, Page: makePage(f.Page, total)}, nil
}

func (r *Repository) RecentJobs(ctx context.Context, limit int) ([]JobView, error) {
	if limit <= 0 || limit > 20 {
		limit = 8
	}
	rows, err := r.db.QueryContext(ctx, jobSelect+` ORDER BY j.updated_at DESC, j.id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []JobView
	for rows.Next() {
		v, err := scanJobView(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, v)
	}
	return items, rows.Err()
}

func jobWhere(f JobFilters) (string, []any) {
	clauses := []string{"1=1"}
	args := make([]any, 0, 4)
	add := func(clause string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(clause, len(args)))
	}
	if f.Status != "" {
		add("j.status = $%d", f.Status)
	}
	if f.Category != "" {
		add("c.category = $%d", f.Category)
	}
	if f.Account != "" {
		add("a.code = $%d", f.Account)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		add("(c.title ILIKE $%[1]d OR c.summary ILIKE $%[1]d OR c.source ILIKE $%[1]d)", "%"+q+"%")
		// The same placeholder is intentionally used for all searchable columns.
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

type NewsView struct {
	ID              int64
	Title           string
	Summary         string
	Source          string
	SourceURL       string
	Category        string
	PublishedAt     sql.NullTime
	CreatedAt       time.Time
	HasScore        bool
	ImportanceScore int
	ViralityScore   int
	RiskLevel       string
	ScoreReason     string
	JobID           sql.NullInt64
	JobStatus       post.Status
}

type NewsFilters struct {
	Category string
	Query    string
	Page     int
}

type NewsPage struct {
	Items []NewsView
	Page  PageInfo
}

func (r *Repository) ListNews(ctx context.Context, f NewsFilters) (NewsPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	clauses := []string{"1=1"}
	args := make([]any, 0, 2)
	if f.Category != "" {
		args = append(args, f.Category)
		clauses = append(clauses, fmt.Sprintf("c.category = $%d", len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		n := len(args)
		clauses = append(clauses, fmt.Sprintf("(c.title ILIKE $%d OR c.summary ILIKE $%d OR c.source ILIKE $%d)", n, n, n))
	}
	where := "WHERE " + strings.Join(clauses, " AND ")
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM news_candidates c `+where, args...).Scan(&total); err != nil {
		return NewsPage{}, err
	}
	args = append(args, pageSize, (f.Page-1)*pageSize)
	query := `SELECT c.id, c.title, c.summary, c.source, c.source_url, c.category, c.published_at, c.created_at,
       (s.id IS NOT NULL), COALESCE(s.importance_score, 0), COALESCE(s.virality_score, 0),
       COALESCE(s.risk_level, ''), COALESCE(s.reason, ''), j.id, COALESCE(j.status, '')
FROM news_candidates c
LEFT JOIN news_scores s ON s.news_candidate_id = c.id
LEFT JOIN LATERAL (
    SELECT id, status FROM post_jobs
    WHERE news_candidate_id = c.id
    ORDER BY id DESC LIMIT 1
) j ON TRUE
` + where + fmt.Sprintf(` ORDER BY c.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return NewsPage{}, err
	}
	defer rows.Close()
	items := make([]NewsView, 0, pageSize)
	for rows.Next() {
		var v NewsView
		if err := rows.Scan(&v.ID, &v.Title, &v.Summary, &v.Source, &v.SourceURL, &v.Category,
			&v.PublishedAt, &v.CreatedAt, &v.HasScore, &v.ImportanceScore, &v.ViralityScore,
			&v.RiskLevel, &v.ScoreReason, &v.JobID, &v.JobStatus); err != nil {
			return NewsPage{}, err
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return NewsPage{}, err
	}
	return NewsPage{Items: items, Page: makePage(f.Page, total)}, nil
}

func makePage(page, total int) PageInfo {
	totalPages := (total + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	return PageInfo{Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages, HasPrev: page > 1, HasNext: page < totalPages}
}

type scanner interface{ Scan(...any) error }

func scanJobView(s scanner) (JobView, error) {
	var v JobView
	err := s.Scan(
		&v.ID, &v.NewsCandidateID, &v.SocialAccountID, &v.Status, &v.RequestedVariantCount,
		&v.SelectedVariantID, &v.InstagramMediaID, &v.AIProvider, &v.AIModel, &v.AIError,
		&v.ErrorMessage, &v.AIRetryCount, &v.NextAIRetryAt, &v.CreatedAt, &v.UpdatedAt,
		&v.Title, &v.Summary, &v.Source, &v.SourceURL, &v.Category, &v.PublishedAt,
		&v.AccountCode, &v.AccountName, &v.InstagramUserID, &v.VariantCount, &v.NotifyThreshold, &v.AccountActive,
		&v.HasScore, &v.ImportanceScore, &v.ViralityScore, &v.AccountFit, &v.ShouldNotify,
		&v.RiskLevel, &v.ScoreReason,
	)
	return v, err
}
