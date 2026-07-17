// Package instagram publishes single-image posts via the Instagram Graph
// Content Publishing API. When publishing is disabled it runs in dry-run mode
// and never contacts the Graph API.
package instagram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"ai-social-publisher/internal/config"
)

// PublishRequest describes a single-image post.
type PublishRequest struct {
	InstagramUserID string
	ImageURL        string
	Caption         string
}

// PublishResult is returned on success. Raw payloads are surfaced for logging.
type PublishResult struct {
	MediaID      string
	CreationID   string
	DryRun       bool
	RequestDump  json.RawMessage
	ResponseDump json.RawMessage
}

// Publisher talks to the Instagram Graph API. The design leaves room for reels /
// carousel later by adding new methods; only single image is implemented now.
type Publisher struct {
	cfg        config.InstagramConfig
	httpClient *http.Client
	logger     *slog.Logger
}

func NewPublisher(cfg config.InstagramConfig, logger *slog.Logger) *Publisher {
	return &Publisher{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		logger:     logger.With("component", "instagram"),
	}
}

// PublishImage runs the two-step Graph flow: create a media container, then
// publish it. In dry-run mode it returns synthetic ids without any HTTP call.
func (p *Publisher) PublishImage(ctx context.Context, req PublishRequest) (*PublishResult, error) {
	reqDump, _ := json.Marshal(map[string]any{
		"instagram_user_id": req.InstagramUserID,
		"image_url":         req.ImageURL,
		"caption_length":    len(req.Caption),
		"publish_enabled":   p.cfg.PublishEnabled,
	})

	if !p.cfg.PublishEnabled {
		p.logger.Info("dry-run: skipping real Instagram publish", "ig_user_id", req.InstagramUserID, "image_url", req.ImageURL)
		mediaID := "dryrun_media_" + fmt.Sprint(time.Now().UnixNano())
		creationID := "dryrun_creation_" + fmt.Sprint(time.Now().UnixNano())
		respDump, _ := json.Marshal(map[string]any{"dry_run": true, "media_id": mediaID, "creation_id": creationID})
		return &PublishResult{
			MediaID:      mediaID,
			CreationID:   creationID,
			DryRun:       true,
			RequestDump:  reqDump,
			ResponseDump: respDump,
		}, nil
	}

	if req.InstagramUserID == "" {
		return nil, fmt.Errorf("instagram user id is empty")
	}
	if p.cfg.AccessToken == "" {
		return nil, fmt.Errorf("instagram access token is not configured")
	}

	// Step 1: create media container.
	creationID, err := p.createMediaContainer(ctx, req)
	if err != nil {
		return nil, err
	}

	// Step 2: publish the container.
	mediaID, respBody, err := p.publishMedia(ctx, req.InstagramUserID, creationID)
	if err != nil {
		// Surface the dumps alongside the error so the publish failure log
		// records what the Graph API actually answered.
		return &PublishResult{
			CreationID:   creationID,
			RequestDump:  reqDump,
			ResponseDump: respBody,
		}, err
	}

	return &PublishResult{
		MediaID:      mediaID,
		CreationID:   creationID,
		DryRun:       false,
		RequestDump:  reqDump,
		ResponseDump: respBody,
	}, nil
}

// graphURL builds /<version>/<ig-user-id>/<endpoint>. The user id is
// path-escaped so a malformed config value cannot alter the request path.
func (p *Publisher) graphURL(igUserID, endpoint string) string {
	return fmt.Sprintf("%s/%s/%s/%s", strings.TrimRight(p.cfg.GraphBaseURL, "/"), p.cfg.APIVersion, url.PathEscape(igUserID), endpoint)
}

func (p *Publisher) createMediaContainer(ctx context.Context, req PublishRequest) (string, error) {
	form := url.Values{}
	form.Set("image_url", req.ImageURL)
	form.Set("caption", req.Caption)
	form.Set("access_token", p.cfg.AccessToken)

	endpoint := p.graphURL(req.InstagramUserID, "media")
	body, err := p.postForm(ctx, endpoint, form)
	if err != nil {
		return "", fmt.Errorf("create media container: %w", err)
	}

	var out struct {
		ID    string `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode media container response: %w", err)
	}
	if out.Error != nil {
		return "", fmt.Errorf("graph error creating container: %s", out.Error.Message)
	}
	if out.ID == "" {
		return "", fmt.Errorf("graph returned empty creation_id")
	}
	return out.ID, nil
}

func (p *Publisher) publishMedia(ctx context.Context, igUserID, creationID string) (string, json.RawMessage, error) {
	form := url.Values{}
	form.Set("creation_id", creationID)
	form.Set("access_token", p.cfg.AccessToken)

	endpoint := p.graphURL(igUserID, "media_publish")
	body, err := p.postForm(ctx, endpoint, form)
	if err != nil {
		return "", nil, fmt.Errorf("publish media: %w", err)
	}

	var out struct {
		ID    string `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", body, fmt.Errorf("decode publish response: %w", err)
	}
	if out.Error != nil {
		return "", body, fmt.Errorf("graph error publishing: %s", out.Error.Message)
	}
	if out.ID == "" {
		return "", body, fmt.Errorf("graph returned empty media id")
	}
	return out.ID, body, nil
}

func (p *Publisher) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	// Note: access_token is in the form body, never logged here.
	if resp.StatusCode >= 400 {
		return body, fmt.Errorf("graph API status %d: %s", resp.StatusCode, truncateBody(body, 500))
	}
	return body, nil
}

// truncateBody bounds an error body and guarantees valid UTF-8: the result is
// persisted into publish_logs/error_message columns, and Postgres rejects
// invalid byte sequences.
func truncateBody(body []byte, limit int) string {
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.ToValidUTF8(strings.TrimSpace(string(body)), "�")
}
