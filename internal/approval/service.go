// Package approval orchestrates the end-to-end flow: score a candidate, request
// the first approval, generate variants on demand, then render, publish and
// report the result. It is the glue between the domain packages.
package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/instagram"
	"ai-social-publisher/internal/media"
	"ai-social-publisher/internal/news"
	"ai-social-publisher/internal/outbox"
	"ai-social-publisher/internal/post"
	"ai-social-publisher/internal/storage"
	"ai-social-publisher/internal/telegram"
)

// Service coordinates the full content pipeline.
type Service struct {
	cfg        *config.Config
	accounts   *account.Repository
	news       *news.Repository
	posts      *post.Repository
	aiSvc      *ai.Service
	telegram   *telegram.Client
	renderer   media.MediaRenderer
	storage    storage.Storage
	publisher  *instagram.Publisher
	newsClient *news.Client
	outbox     *outbox.Repository
	logger     *slog.Logger
}

// Deps bundles the service dependencies.
type Deps struct {
	Config     *config.Config
	Accounts   *account.Repository
	News       *news.Repository
	Posts      *post.Repository
	AI         *ai.Service
	Telegram   *telegram.Client
	Renderer   media.MediaRenderer
	Storage    storage.Storage
	Publisher  *instagram.Publisher
	NewsClient *news.Client
	Outbox     *outbox.Repository
	Logger     *slog.Logger
}

func NewService(d Deps) *Service {
	return &Service{
		cfg:        d.Config,
		accounts:   d.Accounts,
		news:       d.News,
		posts:      d.Posts,
		aiSvc:      d.AI,
		telegram:   d.Telegram,
		renderer:   d.Renderer,
		storage:    d.Storage,
		publisher:  d.Publisher,
		newsClient: d.NewsClient,
		outbox:     d.Outbox,
		logger:     d.Logger.With("component", "approval"),
	}
}

// SyncNews pulls categories concurrently, stores non-duplicate candidates and
// queues scoring. Returns the number of new candidates found.
func (s *Service) SyncNews(ctx context.Context) (int, error) {
	newCount := 0
	var syncErrors []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, category := range s.cfg.NewsService.Categories {
		category := category
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, err := s.newsClient.FetchByCategory(ctx, category)
			if err != nil {
				// One category failing must not stop the others.
				s.logger.Error("news fetch failed", "category", category, "error", err)
				mu.Lock()
				syncErrors = append(syncErrors, err)
				mu.Unlock()
				return
			}
			for _, item := range items {
				if strings.TrimSpace(item.Category) != category {
					err := fmt.Errorf("news item %q category %q does not match requested category %q", item.ID, item.Category, category)
					s.logger.Error("reject mismatched news category", "error", err)
					mu.Lock()
					syncErrors = append(syncErrors, err)
					mu.Unlock()
					continue
				}
				candidate, created, err := s.storeItem(ctx, item)
				if err != nil {
					s.logger.Error("store news item failed", "external_id", item.ID, "error", err)
					mu.Lock()
					syncErrors = append(syncErrors, err)
					mu.Unlock()
					continue
				}
				if !created {
					continue // duplicate, already handled
				}
				mu.Lock()
				newCount++
				mu.Unlock()
				if err := s.ProcessCandidate(ctx, *candidate); err != nil {
					s.logger.Error("process candidate failed", "candidate_id", candidate.ID, "error", err)
					mu.Lock()
					syncErrors = append(syncErrors, err)
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return newCount, errors.Join(syncErrors...)
}

func (s *Service) storeItem(ctx context.Context, item news.Item) (*news.Candidate, bool, error) {
	item.ID = strings.TrimSpace(item.ID)
	item.Title = strings.TrimSpace(item.Title)
	item.Category = strings.TrimSpace(item.Category)
	if item.ID == "" || item.Title == "" || item.Category == "" {
		return nil, false, fmt.Errorf("news item id, title and category are required")
	}
	if len([]rune(item.ID)) > 500 {
		return nil, false, fmt.Errorf("news id exceeds 500 characters")
	}
	if len([]rune(item.Title)) > 500 {
		return nil, false, fmt.Errorf("news title exceeds 500 characters")
	}
	item.Summary = truncateRunes(strings.TrimSpace(item.Summary), 4000)
	item.Source = truncateRunes(strings.TrimSpace(item.Source), 200)
	item.URL = strings.TrimSpace(item.URL)
	if len([]rune(item.URL)) > 2000 {
		return nil, false, fmt.Errorf("news source URL exceeds 2000 characters")
	}
	if item.URL != "" {
		u, err := url.ParseRequestURI(item.URL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, false, fmt.Errorf("news source URL is invalid")
		}
	}
	if _, ok := s.cfg.AccountByCategory(item.Category); !ok {
		return nil, false, fmt.Errorf("unsupported news category %q", item.Category)
	}
	raw, _ := json.Marshal(item)
	c := news.Candidate{
		ExternalNewsID: item.ID,
		Title:          item.Title,
		Summary:        item.Summary,
		Source:         item.Source,
		SourceURL:      item.URL,
		Category:       item.Category,
		RawPayload:     raw,
	}
	if !item.PublishedAt.IsZero() {
		t := item.PublishedAt
		c.PublishedAt = &t
	}
	return s.news.Upsert(ctx, c)
}

// ProcessCandidate creates and queues a job. AI calls are performed by workers,
// never inside an HTTP or Telegram callback request.
func (s *Service) ProcessCandidate(ctx context.Context, candidate news.Candidate) error {
	acct, err := s.accounts.GetByCategory(ctx, candidate.Category)
	if err != nil {
		if errors.Is(err, account.ErrNotFound) {
			s.logger.Debug("no account for category, skipping", "category", candidate.Category)
			return nil
		}
		return err
	}

	job, created, err := s.posts.GetOrCreate(ctx, candidate.ID, acct.ID)
	if err != nil {
		return err
	}
	if !created {
		// Already processed (duplicate control at the job level).
		s.logger.Debug("job already exists, skipping", "job_id", job.ID, "status", job.Status)
		return nil
	}

	return s.posts.UpdateStatus(ctx, job.ID, post.StatusScoringQueued)
}

// scoreJob runs scoring for a NEW/WAITING_AI job and advances it.
func (s *Service) scoreJob(ctx context.Context, job post.Job, candidate news.Candidate, acct account.Account) error {
	result, err := s.aiSvc.ScoreNews(ctx, toAINews(candidate))
	if err != nil {
		if errors.Is(err, ai.ErrAllProvidersFailed) {
			s.logger.Warn("all AI providers failed scoring, parking job", "job_id", job.ID)
			return s.posts.ParkForAIRetry(ctx, job.ID, "scoring: all AI providers failed")
		}
		return err
	}

	score := result.Score
	if _, err := s.news.SaveScore(ctx, news.Score{
		NewsCandidateID: candidate.ID,
		ImportanceScore: score.ImportanceScore,
		ViralityScore:   score.ViralityScore,
		AccountFit:      score.AccountFit,
		ShouldNotify:    score.ShouldNotify,
		RiskLevel:       score.RiskLevel,
		Reason:          score.Reason,
		AIProvider:      result.Provider,
		AIModel:         result.Model,
	}); err != nil {
		return err
	}

	if err := s.posts.ApplyScored(ctx, job.ID, post.ScoreUpdate{
		Status:     post.StatusScored,
		AIProvider: result.Provider,
		AIModel:    result.Model,
	}); err != nil {
		return err
	}

	notify := score.ShouldNotify &&
		score.AccountFit == acct.Category &&
		score.RiskLevel != "high" &&
		score.ImportanceScore >= acct.NotifyThreshold

	if !notify {
		s.logger.Info("news not eligible for notification, skipping",
			"job_id", job.ID, "importance", score.ImportanceScore, "threshold", acct.NotifyThreshold,
			"account_fit", score.AccountFit, "risk", score.RiskLevel, "should_notify", score.ShouldNotify)
		return s.posts.UpdateStatus(ctx, job.ID, post.StatusSkipped)
	}

	if err := s.posts.UpdateStatus(ctx, job.ID, post.StatusWaitingFirstApproval); err != nil {
		return err
	}
	return s.queueFirstApproval(ctx, job.ID, candidate, acct, *score)
}

func (s *Service) queueFirstApproval(ctx context.Context, jobID int64, candidate news.Candidate, acct account.Account, score ai.NewsScore) error {
	msg := fmt.Sprintf("Başlık: %s\nÖzet: %s\nKaynak: %s\nURL: %s\nSkor: %d/100\nRisk: %s\nGerekçe: %s\nKategori: %s",
		candidate.Title, candidate.Summary, candidate.Source, candidate.SourceURL,
		score.ImportanceScore, score.RiskLevel, score.Reason, acct.Code)
	n := telegram.Notification{
		Title:   "🔥 Önemli haber bulundu",
		Message: msg,
		Buttons: []telegram.Button{
			{Text: "Post Hazırla", Action: telegram.ActionGeneratePost, Payload: idStr(jobID)},
			{Text: "Geç", Action: telegram.ActionSkipNews, Payload: idStr(jobID)},
		},
	}
	return s.outbox.Enqueue(ctx, fmt.Sprintf("first-approval:%d", jobID), n)
}

// HandleCallback dispatches an inbound Telegram callback to the right handler.
func (s *Service) HandleCallback(ctx context.Context, cb telegram.Callback) error {
	id, err := strconv.ParseInt(cb.Payload, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid payload %q: %w", cb.Payload, err)
	}

	switch cb.Action {
	case telegram.ActionGeneratePost:
		return s.GenerateVariants(ctx, id)
	case telegram.ActionSkipNews:
		return s.SkipJob(ctx, id)
	case telegram.ActionSelectVariant:
		return s.SelectVariant(ctx, id)
	case telegram.ActionRegenerateVariants:
		return s.RegenerateVariants(ctx, id)
	case telegram.ActionCancel:
		return s.CancelJob(ctx, id)
	default:
		return fmt.Errorf("unknown action %q", cb.Action)
	}
}

// GenerateVariants queues generation and returns quickly. Workers perform the
// expensive AI call.
func (s *Service) GenerateVariants(ctx context.Context, jobID int64) error {
	_, _, acct, err := s.loadJobContext(ctx, jobID)
	if err != nil {
		return err
	}

	count := acct.VariantCount
	if count <= 0 {
		count = s.cfg.PostGeneration.DefaultVariantCount
	}
	if count > s.cfg.PostGeneration.MaxVariantCount {
		count = s.cfg.PostGeneration.MaxVariantCount
	}

	return s.posts.QueueVariants(ctx, jobID, count)
}

// RegenerateVariants re-runs generation for a job awaiting variant approval.
func (s *Service) RegenerateVariants(ctx context.Context, jobID int64) error {
	job, _, acct, err := s.loadJobContext(ctx, jobID)
	if err != nil {
		return err
	}
	count := job.RequestedVariantCount
	if count <= 0 {
		count = acct.VariantCount
	}
	return s.posts.QueueVariants(ctx, jobID, count)
}

func (s *Service) runVariantGeneration(ctx context.Context, job post.Job, candidate news.Candidate, acct account.Account, count int) error {
	styles := s.stylesFor(acct.Category)
	result, err := s.aiSvc.GeneratePostVariants(ctx, ai.GeneratePostVariantsRequest{
		News:         toAINews(candidate),
		Category:     acct.Category,
		VariantCount: count,
		Styles:       styles,
	})
	if err != nil {
		if errors.Is(err, ai.ErrAllProvidersFailed) {
			s.logger.Warn("all AI providers failed generating variants, parking job", "job_id", job.ID)
			if stateErr := s.posts.ParkForAIRetry(ctx, job.ID, "variants: all AI providers failed"); stateErr != nil {
				return stateErr
			}
			current, getErr := s.posts.GetByID(ctx, job.ID)
			if getErr == nil && current.Status == post.StatusFailed {
				return s.queueNotice(ctx, fmt.Sprintf("variants-ai-exhausted:%d", job.ID), "❌ Alternatif üretimi durduruldu", "AI sağlayıcıları art arda yanıt vermedi; job manuel inceleme için FAILED durumuna alındı.", nil)
			}
			return s.queueNotice(ctx, fmt.Sprintf("variants-ai-failed:%d", job.ID), "⚠️ Alternatif üretilemedi", "AI sağlayıcılar şu an yanıt vermiyor. Daha sonra tekrar denenecek.", nil)
		}
		return err
	}

	variants := make([]post.Variant, 0, len(result.Variants))
	for _, v := range result.Variants {
		variants = append(variants, post.Variant{VariantNo: v.VariantNo, Style: v.Style, Caption: v.Caption})
	}

	saved, err := s.posts.ReplaceVariants(ctx, job.ID, variants)
	if err != nil {
		return err
	}
	if err := s.posts.CompleteAIStage(ctx, job.ID, post.StatusWaitingVariantApproval); err != nil {
		return err
	}

	return s.queueVariantApproval(ctx, job.ID, candidate, saved)
}

func (s *Service) queueVariantApproval(ctx context.Context, jobID int64, candidate news.Candidate, variants []post.Variant) error {
	buttons := make([]telegram.Button, 0, len(variants)+2)
	for _, v := range variants {
		if err := s.outbox.Enqueue(ctx, fmt.Sprintf("variant-content:%d:%d", jobID, v.ID), telegram.Notification{
			Title:   fmt.Sprintf("📝 %d. Alternatif — %s", v.VariantNo, candidate.Title),
			Message: fmt.Sprintf("Tarz: %s\n\n%s", v.Style, v.Caption),
		}); err != nil {
			return err
		}
		buttons = append(buttons, telegram.Button{
			Text:    fmt.Sprintf("%d. Alternatif", v.VariantNo),
			Action:  telegram.ActionSelectVariant,
			Payload: idStr(v.ID),
		})
	}
	buttons = append(buttons,
		telegram.Button{Text: "Yeniden Üret", Action: telegram.ActionRegenerateVariants, Payload: idStr(jobID)},
		telegram.Button{Text: "İptal", Action: telegram.ActionCancel, Payload: idStr(jobID)},
	)

	return s.outbox.Enqueue(ctx, fmt.Sprintf("variant-approval:%d:%d", jobID, variants[0].ID), telegram.Notification{
		Title:   "📝 Post alternatifleri hazır",
		Message: fmt.Sprintf("\"%s\" için %d alternatif yukarıda gösterildi. Birini seç:", candidate.Title, len(variants)),
		Buttons: buttons,
	})
}

// SelectVariant approves a chosen variant; the publish worker claims it later.
func (s *Service) SelectVariant(ctx context.Context, variantID int64) error {
	variant, err := s.posts.GetVariantByID(ctx, variantID)
	if err != nil {
		return err
	}
	return s.SelectVariantForJob(ctx, variant.PostJobID, variantID)
}

func (s *Service) SelectVariantForJob(ctx context.Context, jobID, variantID int64) error {
	variant, err := s.posts.GetVariantByID(ctx, variantID)
	if err != nil {
		return err
	}
	if variant.PostJobID != jobID {
		return post.ErrNotFound
	}
	job, err := s.posts.GetByID(ctx, jobID)
	if err != nil {
		return err
	}
	if job.SelectedVariantID.Valid && job.SelectedVariantID.Int64 == variantID &&
		(job.Status == post.StatusApproved || job.Status == post.StatusPublishing || job.Status == post.StatusPublished) {
		return nil
	}
	return s.posts.SelectVariant(ctx, jobID, variantID)
}

// QueuePublish validates that the job has already been approved. APPROVED is
// itself the durable publish queue state consumed by PublishApproved.
func (s *Service) QueuePublish(ctx context.Context, jobID int64) error {
	job, err := s.posts.GetByID(ctx, jobID)
	if err != nil {
		return err
	}
	if job.Status == post.StatusApproved || job.Status == post.StatusPublishing || job.Status == post.StatusPublished {
		return nil
	}
	return fmt.Errorf("%w: %s is not publishable", post.ErrInvalidTransition, job.Status)
}

// SkipJob marks a job skipped (from first approval).
func (s *Service) SkipJob(ctx context.Context, jobID int64) error {
	return s.posts.UpdateStatus(ctx, jobID, post.StatusSkipped)
}

// CancelJob marks a job skipped (from variant approval).
func (s *Service) CancelJob(ctx context.Context, jobID int64) error {
	return s.posts.UpdateStatus(ctx, jobID, post.StatusSkipped)
}

// PublishJob renders the selected variant's image, uploads it, publishes to
// Instagram, records a publish log and notifies the user of the outcome.
func (s *Service) PublishJob(ctx context.Context, jobID int64) error {
	job, candidate, acct, err := s.loadJobContext(ctx, jobID)
	if err != nil {
		return err
	}
	if !job.SelectedVariantID.Valid {
		return fmt.Errorf("job %d has no selected variant", jobID)
	}

	claimed, err := s.posts.ClaimStatus(ctx, jobID, post.StatusApproved, post.StatusPublishing)
	if err != nil {
		return err
	}
	if !claimed {
		current, getErr := s.posts.GetByID(ctx, jobID)
		if getErr != nil {
			return getErr
		}
		if current.Status == post.StatusPublishing || current.Status == post.StatusPublished {
			return nil
		}
		return fmt.Errorf("%w: cannot claim %s for publishing", post.ErrInvalidTransition, current.Status)
	}

	variant, err := s.posts.GetVariantByID(ctx, job.SelectedVariantID.Int64)
	if err != nil {
		return s.fail(ctx, jobID, fmt.Sprintf("load variant: %v", err))
	}

	// Render image.
	rendered, err := s.renderer.RenderPostImage(ctx, *variant, toAINews(*candidate), *acct)
	if err != nil {
		return s.fail(ctx, jobID, fmt.Sprintf("render image: %v", err))
	}

	// Upload to obtain a public URL.
	uploaded, err := s.storage.Upload(ctx, rendered.LocalPath)
	if err != nil {
		return s.fail(ctx, jobID, fmt.Sprintf("upload image: %v", err))
	}
	if err := s.posts.SetVariantImageURL(ctx, variant.ID, uploaded.PublicURL); err != nil {
		s.logger.Warn("failed to persist variant image url", "error", err)
	}

	// Publish (dry-run aware).
	result, pubErr := s.publisher.PublishImage(ctx, instagram.PublishRequest{
		InstagramUserID: acct.InstagramUserID,
		ImageURL:        uploaded.PublicURL,
		Caption:         variant.Caption,
	})

	logEntry := post.PublishLog{PostJobID: jobID, Platform: "instagram"}
	if result != nil {
		logEntry.RequestPayload = result.RequestDump
		logEntry.ResponsePayload = result.ResponseDump
	}
	if pubErr != nil {
		logEntry.Success = false
		logEntry.ErrorMessage = pubErr.Error()
		if err := s.posts.InsertPublishLog(ctx, logEntry); err != nil {
			s.logger.Error("failed to persist publish failure log", "job_id", jobID, "error", err)
		}
		return s.fail(ctx, jobID, fmt.Sprintf("instagram publish: %v", pubErr))
	}

	logEntry.Success = true
	if err := s.posts.InsertPublishLog(ctx, logEntry); err != nil {
		s.logger.Error("failed to persist publish success log", "job_id", jobID, "error", err)
	}

	if err := s.posts.MarkPublished(ctx, jobID, result.MediaID); err != nil {
		return err
	}

	suffix := ""
	if result.DryRun {
		suffix = " (dry-run)"
	}
	return s.queueNotice(ctx, fmt.Sprintf("published:%d", jobID), "✅ Yayınlandı"+suffix,
		fmt.Sprintf("\"%s\" Instagram'da yayınlandı.\nMedia ID: %s", candidate.Title, result.MediaID), nil)
}

// fail records an error message, transitions to FAILED and notifies the user.
func (s *Service) fail(ctx context.Context, jobID int64, msg string) error {
	s.logger.Error("publish job failed", "job_id", jobID, "error", msg)
	if err := s.posts.MarkFailed(ctx, jobID, msg); err != nil {
		s.logger.Error("failed to mark job failed", "job_id", jobID, "error", err)
		return errors.Join(errors.New(msg), err)
	}
	if err := s.queueNotice(ctx, fmt.Sprintf("job-failed:%d", jobID), "❌ Hata oluştu", fmt.Sprintf("Post yayınlanamadı (job #%d): %s", jobID, msg), nil); err != nil {
		s.logger.Error("failed to enqueue failure notification", "job_id", jobID, "error", err)
	}
	return errors.New(msg)
}

// ProcessScoringQueue claims and processes queued scoring work.
func (s *Service) ProcessScoringQueue(ctx context.Context) error {
	jobs, err := s.posts.ListByStatus(ctx, post.StatusScoringQueued, 1)
	if err != nil {
		return err
	}
	for _, queued := range jobs {
		claimed, err := s.posts.ClaimStatus(ctx, queued.ID, post.StatusScoringQueued, post.StatusScoring)
		if err != nil || !claimed {
			if err != nil {
				s.logger.Error("claim scoring job failed", "job_id", queued.ID, "error", err)
			}
			continue
		}
		candidate, acct, err := s.loadCandidateAccount(ctx, queued)
		if err != nil {
			if markErr := s.posts.MarkFailed(ctx, queued.ID, "load scoring context: "+err.Error()); markErr != nil {
				s.logger.Error("mark scoring job failed", "job_id", queued.ID, "error", markErr)
			}
			continue
		}
		if err := s.scoreJob(ctx, queued, *candidate, *acct); err != nil {
			s.logger.Error("score job failed", "job_id", queued.ID, "error", err)
		}
	}
	return nil
}

// ProcessVariantQueue claims and processes queued variant generation work.
func (s *Service) ProcessVariantQueue(ctx context.Context) error {
	jobs, err := s.posts.ListByStatus(ctx, post.StatusVariantsQueued, 1)
	if err != nil {
		return err
	}
	for _, queued := range jobs {
		claimed, err := s.posts.ClaimStatus(ctx, queued.ID, post.StatusVariantsQueued, post.StatusGeneratingVariants)
		if err != nil || !claimed {
			if err != nil {
				s.logger.Error("claim variant job failed", "job_id", queued.ID, "error", err)
			}
			continue
		}
		candidate, acct, err := s.loadCandidateAccount(ctx, queued)
		if err != nil {
			if markErr := s.posts.MarkFailed(ctx, queued.ID, "load variant context: "+err.Error()); markErr != nil {
				s.logger.Error("mark variant job failed", "job_id", queued.ID, "error", markErr)
			}
			continue
		}
		if err := s.runVariantGeneration(ctx, queued, *candidate, *acct, queued.RequestedVariantCount); err != nil {
			s.logger.Error("variant generation failed", "job_id", queued.ID, "error", err)
		}
	}
	return nil
}

// RetryWaitingAI re-attempts jobs parked in WAITING_AI. Jobs that already had a
// variant count requested retry generation; others retry scoring.
func (s *Service) RetryWaitingAI(ctx context.Context) error {
	jobs, err := s.posts.ListByStatus(ctx, post.StatusWaitingAI, 50)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		var target post.Status
		if job.RequestedVariantCount > 0 {
			target = post.StatusVariantsQueued
		} else {
			target = post.StatusScoringQueued
		}
		if _, err := s.posts.ClaimStatus(ctx, job.ID, post.StatusWaitingAI, target); err != nil {
			s.logger.Error("queue AI retry failed", "job_id", job.ID, "error", err)
		}
	}
	return nil
}

// RecoverStaleJobs prevents crashes from leaving work permanently stuck. AI
// stages are safely retried. A stale PUBLISHING job is failed for manual
// reconciliation because automatically replaying an uncertain external publish
// could create a duplicate Instagram post.
func (s *Service) RecoverStaleJobs(ctx context.Context) error {
	cutoff := time.Now().Add(-time.Duration(s.cfg.Scheduler.StaleJobTimeoutMinutes) * time.Minute)
	jobs, err := s.posts.ListStaleProcessing(ctx, cutoff, 50)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		switch job.Status {
		case post.StatusScoring, post.StatusGeneratingVariants:
			if err := s.posts.ParkForAIRetry(ctx, job.ID, "recovered stale worker lease"); err != nil {
				s.logger.Error("recover stale AI job failed", "job_id", job.ID, "error", err)
			}
		case post.StatusPublishing:
			if mediaID, ok, err := s.posts.SuccessfulPublishMediaID(ctx, job.ID); err == nil && ok {
				if err := s.posts.MarkPublished(ctx, job.ID, mediaID); err != nil {
					s.logger.Error("reconcile successful publish failed", "job_id", job.ID, "error", err)
				}
				continue
			}
			if err := s.posts.MarkFailed(ctx, job.ID, "publish result is uncertain after worker interruption; reconcile manually before retrying"); err != nil {
				s.logger.Error("mark uncertain publish failed", "job_id", job.ID, "error", err)
			}
		}
	}
	return nil
}

// PublishApproved publishes any jobs sitting in APPROVED (safety net for the
// scheduler in case the inline publish after selection failed to run).
func (s *Service) PublishApproved(ctx context.Context) error {
	jobs, err := s.posts.ListByStatus(ctx, post.StatusApproved, 20)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err := s.PublishJob(ctx, job.ID); err != nil {
			s.logger.Error("scheduled publish failed", "job_id", job.ID, "error", err)
		}
	}
	return nil
}

// RepairNotifications closes the small crash window between committing a job
// state and enqueueing its notification. Dedupe keys make repeated repairs safe.
func (s *Service) RepairNotifications(ctx context.Context) error {
	first, err := s.posts.ListByStatus(ctx, post.StatusWaitingFirstApproval, 50)
	if err != nil {
		return err
	}
	for _, job := range first {
		candidate, acct, err := s.loadCandidateAccount(ctx, job)
		if err != nil {
			continue
		}
		score, err := s.news.GetLatestScore(ctx, candidate.ID)
		if err != nil {
			continue
		}
		if err := s.queueFirstApproval(ctx, job.ID, *candidate, *acct, ai.NewsScore{
			ImportanceScore: score.ImportanceScore, ViralityScore: score.ViralityScore,
			AccountFit: score.AccountFit, ShouldNotify: score.ShouldNotify,
			RiskLevel: score.RiskLevel, Reason: score.Reason,
		}); err != nil {
			s.logger.Error("repair first approval notification failed", "job_id", job.ID, "error", err)
		}
	}

	variantJobs, err := s.posts.ListByStatus(ctx, post.StatusWaitingVariantApproval, 50)
	if err != nil {
		return err
	}
	for _, job := range variantJobs {
		candidate, _, err := s.loadCandidateAccount(ctx, job)
		if err != nil {
			continue
		}
		variants, err := s.posts.ListVariants(ctx, job.ID)
		if err == nil && len(variants) > 0 {
			if err := s.queueVariantApproval(ctx, job.ID, *candidate, variants); err != nil {
				s.logger.Error("repair variant notification failed", "job_id", job.ID, "error", err)
			}
		}
	}

	publishedJobs, err := s.posts.ListRecentByStatus(ctx, post.StatusPublished, 50)
	if err != nil {
		return err
	}
	for _, job := range publishedJobs {
		candidate, _, err := s.loadCandidateAccount(ctx, job)
		if err == nil {
			if err := s.queueNotice(ctx, fmt.Sprintf("published:%d", job.ID), "✅ Yayınlandı",
				fmt.Sprintf("\"%s\" Instagram'da yayınlandı.\nMedia ID: %s", candidate.Title, job.InstagramMediaID), nil); err != nil {
				s.logger.Error("repair published notification failed", "job_id", job.ID, "error", err)
			}
		}
	}
	failedJobs, err := s.posts.ListRecentByStatus(ctx, post.StatusFailed, 50)
	if err != nil {
		return err
	}
	for _, job := range failedJobs {
		if err := s.queueNotice(ctx, fmt.Sprintf("job-failed:%d", job.ID), "❌ Job durduruldu",
			fmt.Sprintf("Job #%d FAILED durumunda: %s", job.ID, job.ErrorMessage), nil); err != nil {
			s.logger.Error("repair failed notification failed", "job_id", job.ID, "error", err)
		}
	}
	return nil
}

// DeliverNotifications sends leased outbox messages with exponential retry.
func (s *Service) DeliverNotifications(ctx context.Context) error {
	for i := 0; i < 20; i++ {
		message, err := s.outbox.ClaimDue(ctx)
		if err != nil {
			return err
		}
		if message == nil {
			return nil
		}
		if err := s.telegram.Send(ctx, message.Notification); err != nil {
			s.logger.Error("notification delivery failed", "outbox_id", message.ID, "attempt", message.Attempts, "error", err)
			if markErr := s.outbox.MarkFailed(ctx, message.ID, message.Attempts, err); markErr != nil {
				return markErr
			}
			continue
		}
		if err := s.outbox.MarkSent(ctx, message.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---- helpers ----

func (s *Service) loadJobContext(ctx context.Context, jobID int64) (*post.Job, *news.Candidate, *account.Account, error) {
	job, err := s.posts.GetByID(ctx, jobID)
	if err != nil {
		return nil, nil, nil, err
	}
	candidate, acct, err := s.loadCandidateAccount(ctx, *job)
	if err != nil {
		return nil, nil, nil, err
	}
	return job, candidate, acct, nil
}

func (s *Service) loadCandidateAccount(ctx context.Context, job post.Job) (*news.Candidate, *account.Account, error) {
	candidate, err := s.news.GetByID(ctx, job.NewsCandidateID)
	if err != nil {
		return nil, nil, err
	}
	acct, err := s.accounts.GetByID(ctx, job.SocialAccountID)
	if err != nil {
		return nil, nil, err
	}
	return candidate, acct, nil
}

func (s *Service) stylesFor(category string) []string {
	if a, ok := s.cfg.AccountByCategory(category); ok {
		return a.Styles
	}
	return nil
}

func (s *Service) queueNotice(ctx context.Context, key, title, message string, buttons []telegram.Button) error {
	return s.outbox.Enqueue(ctx, key, telegram.Notification{Title: title, Message: message, Buttons: buttons})
}

func toAINews(c news.Candidate) ai.NewsCandidate {
	n := ai.NewsCandidate{
		ID:        c.ExternalNewsID,
		Title:     c.Title,
		Summary:   c.Summary,
		Source:    c.Source,
		SourceURL: c.SourceURL,
		Category:  c.Category,
	}
	if c.PublishedAt != nil {
		n.PublishedAt = *c.PublishedAt
	}
	return n
}

func idStr(id int64) string { return strconv.FormatInt(id, 10) }

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
