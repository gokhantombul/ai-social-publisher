package http

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"ai-social-publisher/internal/account"
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
	default:
		return http.StatusInternalServerError
	}
}

// ---- handlers ----

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) listAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := h.accounts.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": accts})
}

func (h *Handler) createAccount(w http.ResponseWriter, r *http.Request) {
	var in account.Account
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Code == "" || in.Category == "" {
		writeError(w, http.StatusBadRequest, "code and category are required")
		return
	}
	if in.VariantCount <= 0 {
		in.VariantCount = h.cfg.PostGeneration.DefaultVariantCount
	}
	if in.VariantCount > h.cfg.PostGeneration.MaxVariantCount {
		in.VariantCount = h.cfg.PostGeneration.MaxVariantCount
	}
	if in.NotifyThreshold == 0 {
		in.NotifyThreshold = 80
	}
	created, err := h.accounts.Create(r.Context(), in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (h *Handler) syncNews(w http.ResponseWriter, r *http.Request) {
	n, err := h.approval.SyncNews(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"newCandidates": n})
}

func (h *Handler) listCandidates(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.news.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (h *Handler) listPosts(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := h.posts.List(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, statusForRepoError(err), err.Error())
		return
	}
	variants, err := h.posts.ListVariants(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
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
		writeError(w, statusForRepoError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "generating"})
}

type approveRequest struct {
	VariantID int64 `json:"variantId"`
}

func (h *Handler) approvePost(w http.ResponseWriter, r *http.Request) {
	var in approveRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.VariantID == 0 {
		writeError(w, http.StatusBadRequest, "variantId is required")
		return
	}
	if err := h.approval.SelectVariant(r.Context(), in.VariantID); err != nil {
		writeError(w, statusForRepoError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

func (h *Handler) rejectPost(w http.ResponseWriter, r *http.Request) {
	id, err := h.pathID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.approval.SkipJob(r.Context(), id); err != nil {
		writeError(w, statusForRepoError(err), err.Error())
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
	if err := h.approval.PublishJob(r.Context(), id); err != nil {
		writeError(w, statusForRepoError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "publish_attempted"})
}

func (h *Handler) telegramCallback(w http.ResponseWriter, r *http.Request) {
	var cb telegram.Callback
	if err := json.NewDecoder(r.Body).Decode(&cb); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if cb.Action == "" || cb.Payload == "" {
		writeError(w, http.StatusBadRequest, "action and payload are required")
		return
	}
	if err := h.approval.HandleCallback(r.Context(), cb); err != nil {
		h.logger.Error("telegram callback handling failed", "action", cb.Action, "error", err)
		writeError(w, statusForRepoError(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) analyticsPosts(w http.ResponseWriter, r *http.Request) {
	counts, err := h.posts.StatusCounts(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total := 0
	for _, c := range counts {
		total += c
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totalJobs": total,
		"byStatus":  counts,
		"published": counts[string(post.StatusPublished)],
		"failed":    counts[string(post.StatusFailed)],
		"waitingAI": counts[string(post.StatusWaitingAI)],
	})
}
