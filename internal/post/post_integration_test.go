package post_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
	"ai-social-publisher/internal/post"
)

func nilLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestClaimStatusAllowsSingleWorker(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 5})
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
	if err := database.Migrate(db, nilLogger()); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().Format("150405.000000000")
	var accountID, candidateID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO social_accounts (code,name,category,instagram_user_id,variant_count,notify_threshold,is_active) VALUES ($1,$1,$2,'',3,80,TRUE) RETURNING id`, "claim-"+suffix, "claim-"+suffix).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO news_candidates (external_news_id,title,category) VALUES ($1,'claim test',$2) RETURNING id`, "claim-"+suffix, "claim-"+suffix).Scan(&candidateID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM news_candidates WHERE id=$1`, candidateID); err != nil {
			t.Errorf("cleanup candidate: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM social_accounts WHERE id=$1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	}()

	repo := post.NewRepository(db)
	job, _, err := repo.GetOrCreate(ctx, candidateID, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateStatus(ctx, job.ID, post.StatusScoringQueued); err != nil {
		t.Fatal(err)
	}

	var winners atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claimed, err := repo.ClaimStatus(ctx, job.ID, post.StatusScoringQueued, post.StatusScoring)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claimed {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("expected exactly one worker claim, got %d", winners.Load())
	}
}

func TestStaleNewJobIsListedAndRequeueable(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := database.Migrate(db, nilLogger()); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().Format("150405.000000000")
	var accountID, candidateID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO social_accounts (code,name,category,instagram_user_id,variant_count,notify_threshold,is_active) VALUES ($1,$1,$2,'',3,80,TRUE) RETURNING id`, "stale-"+suffix, "stale-"+suffix).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `INSERT INTO news_candidates (external_news_id,title,category) VALUES ($1,'stale new test',$2) RETURNING id`, "stale-"+suffix, "stale-"+suffix).Scan(&candidateID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM news_candidates WHERE id=$1`, candidateID); err != nil {
			t.Errorf("cleanup candidate: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM social_accounts WHERE id=$1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	}()

	repo := post.NewRepository(db)
	job, _, err := repo.GetOrCreate(ctx, candidateID, accountID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between job creation and queueing: the job sits in NEW
	// past the stale cutoff.
	if _, err := db.ExecContext(ctx, `UPDATE post_jobs SET updated_at = now() - interval '1 hour' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	stale, err := repo.ListStaleProcessing(ctx, time.Now().Add(-10*time.Minute), 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range stale {
		if s.ID == job.ID {
			found = true
			if s.Status != post.StatusNew {
				t.Fatalf("expected stale job in NEW, got %s", s.Status)
			}
		}
	}
	if !found {
		t.Fatalf("stale NEW job %d not returned by ListStaleProcessing", job.ID)
	}

	claimed, err := repo.ClaimStatus(ctx, job.ID, post.StatusNew, post.StatusScoringQueued)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed {
		t.Fatal("expected stale NEW job to be claimable into SCORING_QUEUED")
	}
}
