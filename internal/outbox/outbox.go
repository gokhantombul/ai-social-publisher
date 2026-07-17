// Package outbox provides durable, retryable Telegram notification delivery.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"ai-social-publisher/internal/telegram"
)

const maxAttempts = 10

type Message struct {
	ID           int64
	Notification telegram.Notification
	Attempts     int
}

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

// Enqueue is idempotent by dedupeKey.
func (r *Repository) Enqueue(ctx context.Context, dedupeKey string, n telegram.Notification) error {
	if n.IdempotencyKey == "" {
		n.IdempotencyKey = dedupeKey
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal outbox notification: %w", err)
	}
	_, err = r.db.ExecContext(ctx, `
INSERT INTO notification_outbox (dedupe_key, payload)
VALUES ($1, $2)
ON CONFLICT (dedupe_key) DO NOTHING`, dedupeKey, payload)
	return err
}

// minLease is the floor for delivery leases: the lease must comfortably outlast
// one delivery attempt or a second worker could claim (and re-send) a message
// still in flight.
const minLease = time.Minute

// ClaimDue leases one due message for at least lease (clamped up to minLease).
// SKIP LOCKED permits multiple application instances without duplicate delivery.
func (r *Repository) ClaimDue(ctx context.Context, lease time.Duration) (*Message, error) {
	if lease < minLease {
		lease = minLease
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	var m Message
	var payload []byte
	err = tx.QueryRowContext(ctx, `
SELECT id, payload, attempts
FROM notification_outbox
WHERE sent_at IS NULL AND dead_at IS NULL
  AND next_attempt_at <= now()
  AND (locked_until IS NULL OR locked_until < now())
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT 1`).Scan(&m.ID, &payload, &m.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(payload, &m.Notification); err != nil {
		if _, updateErr := tx.ExecContext(ctx, `UPDATE notification_outbox SET dead_at = now(), locked_until = NULL, last_error = $2 WHERE id = $1`, m.ID, "invalid stored payload: "+err.Error()); updateErr != nil {
			return nil, updateErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, commitErr
		}
		return nil, fmt.Errorf("decode outbox payload %d: %w", m.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE notification_outbox
SET locked_until = now() + make_interval(secs => $2), attempts = attempts + 1
WHERE id = $1`, m.ID, lease.Seconds()); err != nil {
		return nil, err
	}
	m.Attempts++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *Repository) MarkSent(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx, `
UPDATE notification_outbox
SET sent_at = now(), locked_until = NULL, last_error = ''
WHERE id = $1`, id)
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, id int64, attempts int, deliveryErr error) error {
	msg := deliveryErr.Error()
	if len(msg) > 1000 {
		msg = msg[:1000]
	}
	// Byte-level truncation can split a UTF-8 sequence; Postgres rejects invalid
	// UTF-8, which would make this bookkeeping write itself fail forever.
	msg = strings.ToValidUTF8(msg, "�")
	if attempts >= maxAttempts {
		_, err := r.db.ExecContext(ctx, `
UPDATE notification_outbox
SET dead_at = now(), locked_until = NULL, last_error = $2
WHERE id = $1`, id, msg)
		return err
	}
	delay := time.Minute << min(attempts-1, 6)
	_, err := r.db.ExecContext(ctx, `
UPDATE notification_outbox
SET next_attempt_at = $2, locked_until = NULL, last_error = $3
WHERE id = $1`, id, time.Now().Add(delay), msg)
	return err
}

// PurgeSent deletes delivered messages older than cutoff so the outbox stays
// small. Dead messages are kept for operator inspection. Callers must keep the
// retention comfortably longer than any notification-repair lookback, otherwise
// repair could re-enqueue (and re-send) a purged dedupe key.
func (r *Repository) PurgeSent(ctx context.Context, cutoff time.Time) (int64, error) {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM notification_outbox WHERE sent_at IS NOT NULL AND sent_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (r *Repository) Counts(ctx context.Context) (pending, dead int, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT
    COUNT(*) FILTER (WHERE sent_at IS NULL AND dead_at IS NULL),
    COUNT(*) FILTER (WHERE dead_at IS NOT NULL)
FROM notification_outbox`).Scan(&pending, &dead)
	return
}
