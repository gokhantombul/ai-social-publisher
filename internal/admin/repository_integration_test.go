package admin

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/database"
)

// TestListQueriesReturnPageAndTotal exercises the single-scan pagination
// queries (COUNT(*) OVER()) against a real database, including the empty-page
// fallback path.
func TestListQueriesReturnPageAndTotal(t *testing.T) {
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
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := database.Migrate(db, logger); err != nil {
		t.Fatal(err)
	}

	suffix := time.Now().Format("150405.000000000")
	var accountID int64
	if err := db.QueryRowContext(ctx, `INSERT INTO social_accounts (code,name,category,instagram_user_id,variant_count,notify_threshold,is_active) VALUES ($1,$1,$2,'',3,80,TRUE) RETURNING id`, "adminrepo-"+suffix, "adminrepo-"+suffix).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	candidateIDs := make([]int64, 0, 2)
	for _, name := range []string{"a", "b"} {
		var id int64
		if err := db.QueryRowContext(ctx, `INSERT INTO news_candidates (external_news_id,title,summary,category) VALUES ($1,$2,'needle summary',$3) RETURNING id`,
			"adminrepo-"+name+"-"+suffix, "adminrepo title "+name, "adminrepo-"+suffix).Scan(&id); err != nil {
			t.Fatal(err)
		}
		candidateIDs = append(candidateIDs, id)
		if _, err := db.ExecContext(ctx, `INSERT INTO post_jobs (news_candidate_id, social_account_id, status) VALUES ($1,$2,'NEW')`, id, accountID); err != nil {
			t.Fatal(err)
		}
	}
	defer func() {
		for _, id := range candidateIDs {
			if _, err := db.ExecContext(ctx, `DELETE FROM news_candidates WHERE id=$1`, id); err != nil {
				t.Errorf("cleanup candidate: %v", err)
			}
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM social_accounts WHERE id=$1`, accountID); err != nil {
			t.Errorf("cleanup account: %v", err)
		}
	}()

	repo := NewRepository(db)

	jobs, err := repo.ListJobs(ctx, JobFilters{Account: "adminrepo-" + suffix, Query: "adminrepo title", Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 || jobs.Page.Total != 2 || jobs.Page.TotalPages != 1 {
		t.Fatalf("unexpected job page: items=%d page=%+v", len(jobs.Items), jobs.Page)
	}

	// Beyond the last page the window total is absent; the fallback count must
	// still report the real total.
	empty, err := repo.ListJobs(ctx, JobFilters{Account: "adminrepo-" + suffix, Page: 99})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Items) != 0 || empty.Page.Total != 2 {
		t.Fatalf("expected empty page with total 2, got items=%d page=%+v", len(empty.Items), empty.Page)
	}

	news, err := repo.ListNews(ctx, NewsFilters{Category: "adminrepo-" + suffix, Query: "needle", Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(news.Items) != 2 || news.Page.Total != 2 {
		t.Fatalf("unexpected news page: items=%d page=%+v", len(news.Items), news.Page)
	}
	if !news.Items[0].JobID.Valid {
		t.Fatal("expected latest job to be joined onto the news row")
	}
}
