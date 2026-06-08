// Package account manages social_accounts persistence and config sync.
package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ai-social-publisher/internal/config"
)

// ErrNotFound is returned when an account lookup yields no row.
var ErrNotFound = errors.New("account not found")

// Account mirrors the social_accounts table.
type Account struct {
	ID              int64     `json:"id"`
	Code            string    `json:"code"`
	Name            string    `json:"name"`
	Category        string    `json:"category"`
	InstagramUserID string    `json:"instagramUserId"`
	VariantCount    int       `json:"variantCount"`
	NotifyThreshold int       `json:"notifyThreshold"`
	AutoPublish     bool      `json:"autoPublish"`
	IsActive        bool      `json:"isActive"`
	CreatedAt       time.Time `json:"createdAt"`
	UpdatedAt       time.Time `json:"updatedAt"`
}

// Repository provides persistence for accounts.
type Repository struct {
	db *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{db: db}
}

// SyncFromConfig upserts the configured accounts into the database by code. It
// is called on startup so config remains the source of truth for channels.
func (r *Repository) SyncFromConfig(ctx context.Context, accounts []config.AccountConfig) error {
	const q = `
INSERT INTO social_accounts (code, name, category, instagram_user_id, variant_count, notify_threshold, auto_publish, is_active, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE, now())
ON CONFLICT (code) DO UPDATE SET
    name = EXCLUDED.name,
    category = EXCLUDED.category,
    instagram_user_id = EXCLUDED.instagram_user_id,
    variant_count = EXCLUDED.variant_count,
    notify_threshold = EXCLUDED.notify_threshold,
    auto_publish = EXCLUDED.auto_publish,
    is_active = TRUE,
    updated_at = now();`

	for _, a := range accounts {
		if _, err := r.db.ExecContext(ctx, q,
			a.Code, a.Name, a.Category, a.InstagramUserID,
			a.VariantCount, a.NotifyThreshold, a.AutoPublish,
		); err != nil {
			return fmt.Errorf("upsert account %q: %w", a.Code, err)
		}
	}
	return nil
}

func (r *Repository) List(ctx context.Context) ([]Account, error) {
	const q = `SELECT id, code, name, category, instagram_user_id, variant_count, notify_threshold, auto_publish, is_active, created_at, updated_at
FROM social_accounts ORDER BY id`
	rows, err := r.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Account
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repository) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.getBy(ctx, "id", id)
}

func (r *Repository) GetByCategory(ctx context.Context, category string) (*Account, error) {
	return r.getBy(ctx, "category", category)
}

func (r *Repository) GetByCode(ctx context.Context, code string) (*Account, error) {
	return r.getBy(ctx, "code", code)
}

func (r *Repository) getBy(ctx context.Context, column string, value any) (*Account, error) {
	q := fmt.Sprintf(`SELECT id, code, name, category, instagram_user_id, variant_count, notify_threshold, auto_publish, is_active, created_at, updated_at
FROM social_accounts WHERE %s = $1 AND is_active = TRUE LIMIT 1`, column)
	a, err := scan(r.db.QueryRowContext(ctx, q, value))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Create inserts a new account (used by the POST /api/accounts endpoint).
func (r *Repository) Create(ctx context.Context, a Account) (*Account, error) {
	const q = `
INSERT INTO social_accounts (code, name, category, instagram_user_id, variant_count, notify_threshold, auto_publish, is_active)
VALUES ($1, $2, $3, $4, $5, $6, $7, TRUE)
RETURNING id, code, name, category, instagram_user_id, variant_count, notify_threshold, auto_publish, is_active, created_at, updated_at`
	created, err := scan(r.db.QueryRowContext(ctx, q,
		a.Code, a.Name, a.Category, a.InstagramUserID, a.VariantCount, a.NotifyThreshold, a.AutoPublish))
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// scanner is satisfied by *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scan(s scanner) (Account, error) {
	var a Account
	err := s.Scan(
		&a.ID, &a.Code, &a.Name, &a.Category, &a.InstagramUserID,
		&a.VariantCount, &a.NotifyThreshold, &a.AutoPublish, &a.IsActive,
		&a.CreatedAt, &a.UpdatedAt,
	)
	return a, err
}
