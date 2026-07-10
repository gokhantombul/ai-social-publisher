package http

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ai-social-publisher/internal/account"
	"ai-social-publisher/internal/approval"
	"ai-social-publisher/internal/post"
	"ai-social-publisher/internal/telegram"

	"github.com/go-chi/chi/v5"
)

// ---- generic JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain a single JSON object")
	}
	return nil
}

func (h *Handler) pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
}

// statusForRepoError maps domain errors to HTTP codes.
func statusForRepoError(err error) int {
	switch {
	case errors.Is(err, post.ErrNotFound), errors.Is(err, account.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, post.ErrInvalidTransition):
		return http.StatusConflict
	case errors.Is(err, approval.ErrInvalidSchedule):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func (h *Handler) writeHandlerError(w http.ResponseWriter, err error) {
	status := statusForRepoError(err)
	if status >= 500 {
		h.logger.Error("request failed", "error", err)
		writeError(w, status, "internal server error")
		return
	}
	writeError(w, status, err.Error())
}

// ---- handlers ----

func (h *Handler) live(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if h.db == nil || h.db.PingContext(ctx) != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := h.accounts.List(r.Context())
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": accts})
}

func (h *Handler) syncNews(w http.ResponseWriter, r *http.Request) {
	n, err := h.approval.SyncNews(r.Context())
	if err != nil {
		h.logger.Error("news sync partially failed", "new_candidates", n, "error", err)
		if n > 0 {
			writeJSON(w, http.StatusMultiStatus, map[string]any{"newCandidates": n, "warning": "one or more categories failed"})
			return
		}
		writeError(w, http.StatusBadGateway, "news sync failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"newCandidates": n})
}

func (h *Handler) listCandidates(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.news.List(r.Context(), limit)
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listPosts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.posts.List(r.Context(), limit)
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) getPost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	job, err := h.posts.GetByID(r.Context(), id)
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	variants, err := h.posts.ListVariants(r.Context(), id)
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job, "variants": variants})
}

func (h *Handler) generatePost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.approval.GenerateVariants(r.Context(), id); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "variants_queued"})
}

type approveRequest struct {
	VariantID int64 `json:"variantId"`
}

func (h *Handler) approvePost(w http.ResponseWriter, r *http.Request) {
	jobID, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in approveRequest
	if err := decodeJSON(w, r, &in); err != nil || in.VariantID == 0 {
		writeError(w, http.StatusBadRequest, "variantId is required")
		return
	}
	if err := h.approval.SelectVariantForJob(r.Context(), jobID, in.VariantID); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "approved"})
}

func (h *Handler) rejectPost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.approval.SkipJob(r.Context(), id); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "skipped"})
}

func (h *Handler) publishPost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.approval.QueuePublish(r.Context(), id); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "publish_queued"})
}

type scheduleRequest struct {
	ScheduledAt string `json:"scheduledAt"`
}

// schedulePost defers publishing of a reviewed job to a future time. The body
// carries an RFC3339 timestamp: {"scheduledAt": "2026-07-11T09:00:00Z"}.
func (h *Handler) schedulePost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var in scheduleRequest
	if err := decodeJSON(w, r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "scheduledAt is required (RFC3339)")
		return
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(in.ScheduledAt))
	if err != nil {
		writeError(w, http.StatusBadRequest, "scheduledAt must be an RFC3339 timestamp")
		return
	}
	if err := h.approval.SchedulePublish(r.Context(), id, at); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scheduled"})
}

// unschedulePost cancels a pending schedule, returning the job to review.
func (h *Handler) unschedulePost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.approval.CancelScheduledPublish(r.Context(), id); err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unscheduled"})
}

func (h *Handler) telegramCallback(w http.ResponseWriter, r *http.Request) {
	var cb telegram.Callback
	if err := decodeJSON(w, r, &cb); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if cb.Action == "" || cb.Payload == "" {
		writeError(w, http.StatusBadRequest, "action and payload are required")
		return
	}
	if !validTelegramAction(cb.Action) {
		writeError(w, http.StatusBadRequest, "unknown callback action")
		return
	}
	if id, err := strconv.ParseInt(cb.Payload, 10, 64); err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid callback payload")
		return
	}
	if !h.telegramUserAllowed(cb.User) {
		writeError(w, http.StatusForbidden, "telegram user is not allowed")
		return
	}
	if err := h.approval.HandleCallback(r.Context(), cb); err != nil {
		h.logger.Error("telegram callback handling failed", "action", cb.Action, "error", err)
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) analyticsPosts(w http.ResponseWriter, r *http.Request) {
	counts, err := h.posts.StatusCounts(r.Context())
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	pendingNotifications, deadNotifications, err := h.outbox.Counts(r.Context())
	if err != nil {
		h.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totalJobs":     total,
		"byStatus":      counts,
		"published":     counts[string(post.StatusPublished)],
		"failed":        counts[string(post.StatusFailed)],
		"waitingAI":     counts[string(post.StatusWaitingAI)],
		"notifications": map[string]int{"pending": pendingNotifications, "dead": deadNotifications},
	})
}

func (h *Handler) telegramUserAllowed(user string) bool {
	for _, allowed := range h.cfg.Security.AllowedTelegramUsers {
		if secureEqual(user, allowed) {
			return true
		}
	}
	return false
}

func validTelegramAction(action string) bool {
	switch action {
	case telegram.ActionGeneratePost, telegram.ActionSkipNews, telegram.ActionSelectVariant,
		telegram.ActionRegenerateVariants, telegram.ActionCancel:
		return true
	default:
		return false
	}
}
