package outbox_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/telegram"
)

func TestOutboxLeaseRetryAndDedupe(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, config.DatabaseConfig{URL: dsn, MaxOpenConns: 4, MaxIdleConns: 1, ConnMaxLifetimeMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lockConn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.ExecContext(ctx, `SELECT pg_advisory_lock(6599187)`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = lockConn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(6599187)`)
		_ = lockConn.Close()
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := database.Migrate(db, logger); err != nil {
		t.Fatal(err)
	}
	repo := outbox.NewRepository(db)
	key := "outbox-test-" + time.Now().Format("150405.000000000")
	defer func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM notification_outbox WHERE dedupe_key=$1`, key); err != nil {
			t.Errorf("cleanup outbox: %v", err)
		}
	}()
	n := telegram.Notification{Title: "test", Message: "message"}
	if err := repo.Enqueue(ctx, key, n); err != nil {
		t.Fatal(err)
	}
	if err := repo.Enqueue(ctx, key, n); err != nil {
		t.Fatal(err)
	}
	message, err := repo.ClaimDue(ctx)
	if err != nil || message == nil || message.Notification.IdempotencyKey != key {
		t.Fatalf("message=%+v err=%v", message, err)
	}
	second, err := repo.ClaimDue(ctx)
	if err != nil || second != nil {
		t.Fatalf("leased message was claimed twice: %+v err=%v", second, err)
	}
	if err := repo.MarkFailed(ctx, message.ID, message.Attempts, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE notification_outbox SET next_attempt_at=now() WHERE id=$1`, message.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := repo.ClaimDue(ctx)
	if err != nil || retry == nil || retry.Attempts != 2 {
		t.Fatalf("retry=%+v err=%v", retry, err)
	}
	if err := repo.MarkSent(ctx, retry.ID); err != nil {
		t.Fatal(err)
	}
	var sent bool
	if err := db.QueryRowContext(ctx, `SELECT sent_at IS NOT NULL FROM notification_outbox WHERE id=$1`, retry.ID).Scan(&sent); err != nil || !sent {
		t.Fatalf("message was not marked sent: sent=%v err=%v", sent, err)
	}
}
