package post_test

import (
	"context"
	"os"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
	"ai-social-publisher/internal/post"
)

// TestScheduledPublishRepository exercises the deferred-publish repository
// methods against a real database: scheduling, the atomic due-claim (which must
// promote exactly once), rescheduling, and cancellation.
func TestScheduledPublishRepository(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	db, err := database.Connect(ctx, config.DatabaseConfig{URL: dsn, MaxOpenConns: 5, MaxIdleConns: 2, ConnMaxLifetimeMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	// Registered first so it runs last: after every candidate/account cleanup.
	t.Cleanup(func() { _ = db.Close() })

	// Share the integration-test advisory lock so this test never runs against
	// the database concurrently with the other integration tests.
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

	repo := post.NewRepository(db)
	suffix := time.Now().Format("150405.000000000")

	var accountID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO social_accounts (code,name,category,instagram_user_id,variant_count,notify_threshold,is_active) VALUES ($1,$1,$2,'',3,80,TRUE) RETURNING id`, "sched-"+suffix, "sched-"+suffix).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(), `DELETE FROM social_accounts WHERE id=$1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	})

	// seedReadyJob returns a job sitting in READY_TO_PUBLISH with a selected variant.
	seedReadyJob := func(t *testing.T, tag string) int64 {
		t.Helper()
		var candidateID int64
		if err := db.QueryRowContext(ctx, `INSERT INTO news_candidates (external_news_id,title,category) VALUES ($1,'sched test',$2) RETURNING id`, tag, "sched-"+suffix).Scan(&candidateID); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := db.ExecContext(context.Background(), `DELETE FROM news_candidates WHERE id=$1`, candidateID); err != nil {
				t.Errorf("cleanup candidate: %v", err)
			}
		})
		job, _, err := repo.GetOrCreate(ctx, candidateID, accountID)
		if err != nil {
			t.Fatal(err)
		}
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(repo.UpdateStatus(ctx, job.ID, post.StatusScoringQueued))
		if ok, err := repo.ClaimStatus(ctx, job.ID, post.StatusScoringQueued, post.StatusScoring); err != nil || !ok {
			t.Fatalf("claim scoring: ok=%v err=%v", ok, err)
		}
		must(repo.ApplyScored(ctx, job.ID, post.ScoreUpdate{Status: post.StatusScored, AIProvider: "test", AIModel: "test"}))
		must(repo.UpdateStatus(ctx, job.ID, post.StatusWaitingFirstApproval))
		must(repo.QueueVariants(ctx, job.ID, 2))
		if ok, err := repo.ClaimStatus(ctx, job.ID, post.StatusVariantsQueued, post.StatusGeneratingVariants); err != nil || !ok {
			t.Fatalf("claim variants: ok=%v err=%v", ok, err)
		}
		saved, err := repo.ReplaceVariants(ctx, job.ID, []post.Variant{
			{VariantNo: 1, Style: "s", Caption: "caption one"},
			{VariantNo: 2, Style: "s", Caption: "caption two"},
		})
		must(err)
		must(repo.CompleteAIStage(ctx, job.ID, post.StatusWaitingVariantApproval))
		must(repo.SelectVariant(ctx, job.ID, saved[0].ID))
		return job.ID
	}

	now := time.Now()

	t.Run("schedule then claim when due", func(t *testing.T) {
		jobID := seedReadyJob(t, "due-"+suffix)
		if err := repo.Schedule(ctx, jobID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetByID(ctx, jobID)
		if err != nil || got.Status != post.StatusScheduled || got.ScheduledPublishAt == nil {
			t.Fatalf("expected SCHEDULED with a time, got %+v err=%v", got, err)
		}
		if due, err := repo.ListDueScheduled(ctx, now, 10); err != nil || containsJob(due, jobID) {
			t.Fatalf("future job must not be due, err=%v", err)
		}
		if claimed, err := repo.ClaimScheduledDue(ctx, jobID, now); err != nil || claimed {
			t.Fatalf("future job must not be claimable: claimed=%v err=%v", claimed, err)
		}

		// The wall clock reaching the target is simulated by moving it into the past.
		if err := repo.Reschedule(ctx, jobID, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
		if due, err := repo.ListDueScheduled(ctx, now, 10); err != nil || !containsJob(due, jobID) {
			t.Fatalf("rescheduled job should be due, err=%v", err)
		}
		if claimed, err := repo.ClaimScheduledDue(ctx, jobID, now); err != nil || !claimed {
			t.Fatalf("due job should be claimed once: claimed=%v err=%v", claimed, err)
		}
		promoted, err := repo.GetByID(ctx, jobID)
		if err != nil || promoted.Status != post.StatusApproved {
			t.Fatalf("expected APPROVED after claim, got %+v err=%v", promoted, err)
		}
		if promoted.ScheduledPublishAt == nil {
			t.Fatal("scheduled time should be retained for audit after promotion")
		}
		if claimed, err := repo.ClaimScheduledDue(ctx, jobID, now); err != nil || claimed {
			t.Fatalf("second claim must fail: claimed=%v err=%v", claimed, err)
		}
	})

	t.Run("cancel returns to review and clears the time", func(t *testing.T) {
		jobID := seedReadyJob(t, "cancel-"+suffix)
		if err := repo.Schedule(ctx, jobID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := repo.CancelSchedule(ctx, jobID); err != nil {
			t.Fatal(err)
		}
		got, err := repo.GetByID(ctx, jobID)
		if err != nil || got.Status != post.StatusReadyToPublish {
			t.Fatalf("expected READY_TO_PUBLISH, got %+v err=%v", got, err)
		}
		if got.ScheduledPublishAt != nil {
			t.Fatal("scheduled time should be cleared after cancel")
		}
		if err := repo.CancelSchedule(ctx, jobID); err == nil {
			t.Fatal("cancel on a non-scheduled job should fail")
		}
	})

	t.Run("schedule rejects an already scheduled job", func(t *testing.T) {
		jobID := seedReadyJob(t, "reject-"+suffix)
		if err := repo.Schedule(ctx, jobID, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := repo.Schedule(ctx, jobID, now.Add(2*time.Hour)); err == nil {
			t.Fatal("expected SCHEDULED->SCHEDULED to be rejected")
		}
	})
}

func containsJob(jobs []post.Job, id int64) bool {
	for _, j := range jobs {
		if j.ID == id {
			return true
		}
	}
	return false
}
