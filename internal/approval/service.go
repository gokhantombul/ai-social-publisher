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
	"strconv"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/ai"
	"ai-social-publisher/internal/config"
	"ai-social-publisher/internal/instagram"
	"ai-social-publisher/internal/media"
	"ai-social-publisher/internal/news"
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
		logger:     d.Logger.With("component", "approval"),
	}
}

// SyncNews pulls news for all configured categories, stores new (non-duplicate)
// candidates and scores each one. Returns the number of new candidates found.
func (s *Service) SyncNews(ctx context.Context) (int, error) {
	newCount := 0
	for _, category := range s.cfg.NewsService.Categories {
		items, err := s.newsClient.FetchByCategory(ctx, category)
		if err != nil {
			// One category failing must not stop the others.
			s.logger.Error("news fetch failed", "category", category, "error", err)
			continue
		}
		for _, item := range items {
			candidate, created, err := s.storeItem(ctx, item)
			if err != nil {
				s.logger.Error("store news item failed", "external_id", item.ID, "error", err)
				continue
			}
			if !created {
				continue // duplicate, already handled
			}
			newCount++
			if err := s.ProcessCandidate(ctx, *candidate); err != nil {
				s.logger.Error("process candidate failed", "candidate_id", candidate.ID, "error", err)
			}
		}
	}
	return newCount, nil
}

func (s *Service) storeItem(ctx context.Context, item news.Item) (*news.Candidate, bool, error) {
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

// ProcessCandidate scores a freshly stored news candidate, creating its post job
// and (if important enough) sending the first-approval Telegram notification.
// AI failures move the job to WAITING_AI rather than failing the pipeline.
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

	return s.scoreJob(ctx, *job, candidate, *acct)
}

// scoreJob runs scoring for a NEW/WAITING_AI job and advances it.
func (s *Service) scoreJob(ctx context.Context, job post.Job, candidate news.Candidate, acct account.Account) error {
	result, err := s.aiSvc.ScoreNews(ctx, toAINews(candidate))
	if err != nil {
		if errors.Is(err, ai.ErrAllProvidersFailed) {
			s.logger.Warn("all AI providers failed scoring, parking job", "job_id", job.ID)
			return s.posts.ApplyScored(ctx, job.ID, post.ScoreUpdate{
				Status:  post.StatusWaitingAI,
				AIError: "scoring: all AI providers failed",
			})
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
		score.AccountFit != "skip" &&
		score.ImportanceScore >= acct.NotifyThreshold

	if !notify {
		s.logger.Info("news below notify threshold, skipping",
			"job_id", job.ID, "importance", score.ImportanceScore, "threshold", acct.NotifyThreshold)
		return s.posts.UpdateStatus(ctx, job.ID, post.StatusSkipped)
	}

	if err := s.posts.UpdateStatus(ctx, job.ID, post.StatusWaitingFirstApproval); err != nil {
		return err
	}
	return s.sendFirstApproval(ctx, job.ID, candidate, acct, *score)
}

func (s *Service) sendFirstApproval(ctx context.Context, jobID int64, candidate news.Candidate, acct account.Account, score ai.NewsScore) error {
	msg := fmt.Sprintf("Başlık: %s\nSkor: %d/100\nKategori: %s", candidate.Title, score.ImportanceScore, acct.Code)
	n := telegram.Notification{
		Title:   "🔥 Önemli haber bulundu",
		Message: msg,
		Buttons: []telegram.Button{
			{Text: "Post Hazırla", Action: telegram.ActionGeneratePost, Payload: idStr(jobID)},
			{Text: "Geç", Action: telegram.ActionSkipNews, Payload: idStr(jobID)},
		},
	}
	if err := s.telegram.Send(ctx, n); err != nil {
		// Telegram failure must not break the pipeline; log and continue.
		s.logger.Error("failed to send first-approval notification", "job_id", jobID, "error", err)
	}
	return nil
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

// GenerateVariants produces config-driven variant alternatives for a job and
// sends the dynamic selection buttons.
func (s *Service) GenerateVariants(ctx context.Context, jobID int64) error {
	job, candidate, acct, err := s.loadJobContext(ctx, jobID)
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

	if err := s.posts.SetRequestedVariantCount(ctx, jobID, count); err != nil {
		return err
	}
	return s.runVariantGeneration(ctx, *job, *candidate, *acct, count)
}

// RegenerateVariants re-runs generation for a job awaiting variant approval.
func (s *Service) RegenerateVariants(ctx context.Context, jobID int64) error {
	job, candidate, acct, err := s.loadJobContext(ctx, jobID)
	if err != nil {
		return err
	}
	count := job.RequestedVariantCount
	if count <= 0 {
		count = acct.VariantCount
	}
	// Move back into generating state (allowed from WAITING_VARIANT_APPROVAL).
	if err := s.posts.UpdateStatus(ctx, jobID, post.StatusGeneratingVariants); err != nil {
		return err
	}
	return s.runVariantGeneration(ctx, *job, *candidate, *acct, count)
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
			_ = s.posts.ApplyScored(ctx, job.ID, post.ScoreUpdate{Status: post.StatusWaitingAI, AIError: "variants: all AI providers failed"})
			s.notify(ctx, "⚠️ Alternatif üretilemedi", "AI sağlayıcılar şu an yanıt vermiyor. Daha sonra tekrar denenecek.", nil)
			return nil
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
	if err := s.posts.UpdateStatus(ctx, job.ID, post.StatusWaitingVariantApproval); err != nil {
		return err
	}

	return s.sendVariantApproval(ctx, job.ID, candidate, saved)
}

func (s *Service) sendVariantApproval(ctx context.Context, jobID int64, candidate news.Candidate, variants []post.Variant) error {
	buttons := make([]telegram.Button, 0, len(variants)+2)
	for _, v := range variants {
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

	msg := fmt.Sprintf("\"%s\" için %d alternatif hazır. Birini seç:", candidate.Title, len(variants))
	s.notify(ctx, "📝 Post alternatifleri hazır", msg, buttons)
	return nil
}

// SelectVariant approves a chosen variant and immediately runs publishing.
func (s *Service) SelectVariant(ctx context.Context, variantID int64) error {
	variant, err := s.posts.GetVariantByID(ctx, variantID)
	if err != nil {
		return err
	}
	if err := s.posts.SelectVariant(ctx, variant.PostJobID, variantID); err != nil {
		return err
	}
	return s.PublishJob(ctx, variant.PostJobID)
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

	if err := s.posts.UpdateStatus(ctx, jobID, post.StatusPublishing); err != nil {
		return err
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
		_ = s.posts.InsertPublishLog(ctx, logEntry)
		return s.fail(ctx, jobID, fmt.Sprintf("instagram publish: %v", pubErr))
	}

	logEntry.Success = true
	_ = s.posts.InsertPublishLog(ctx, logEntry)

	if err := s.posts.MarkPublished(ctx, jobID, result.MediaID); err != nil {
		return err
	}

	suffix := ""
	if result.DryRun {
		suffix = " (dry-run)"
	}
	s.notify(ctx, "✅ Yayınlandı"+suffix,
		fmt.Sprintf("\"%s\" Instagram'da yayınlandı.\nMedia ID: %s", candidate.Title, result.MediaID), nil)
	return nil
}

// fail records an error message, transitions to FAILED and notifies the user.
func (s *Service) fail(ctx context.Context, jobID int64, msg string) error {
	s.logger.Error("publish job failed", "job_id", jobID, "error", msg)
	if err := s.posts.MarkFailed(ctx, jobID, msg); err != nil {
		s.logger.Error("failed to mark job failed", "job_id", jobID, "error", err)
	}
	s.notify(ctx, "❌ Hata oluştu", fmt.Sprintf("Post yayınlanamadı (job #%d): %s", jobID, msg), nil)
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
		candidate, acct, err := s.loadCandidateAccount(ctx, job)
		if err != nil {
			s.logger.Error("retry: failed to load context", "job_id", job.ID, "error", err)
			continue
		}
		if job.RequestedVariantCount > 0 {
			if err := s.runVariantGeneration(ctx, job, *candidate, *acct, job.RequestedVariantCount); err != nil {
				s.logger.Error("retry variant generation failed", "job_id", job.ID, "error", err)
			}
			continue
		}
		if err := s.scoreJob(ctx, job, *candidate, *acct); err != nil {
			s.logger.Error("retry scoring failed", "job_id", job.ID, "error", err)
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

func (s *Service) notify(ctx context.Context, title, message string, buttons []telegram.Button) {
	if err := s.telegram.Send(ctx, telegram.Notification{Title: title, Message: message, Buttons: buttons}); err != nil {
		s.logger.Error("telegram notification failed", "title", title, "error", err)
	}
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
