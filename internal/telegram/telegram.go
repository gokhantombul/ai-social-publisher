// Package telegram is the client for the external telegram-servisi. It sends
// notification messages with action buttons; user callbacks arrive back via the
// HTTP /api/telegram/callback endpoint (handled elsewhere).
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"ai-social-publisher/internal/config"
)

// Action values carried by buttons and returned in callbacks.
const (
	ActionGeneratePost       = "GENERATE_POST"
	ActionSkipNews           = "SKIP_NEWS"
	ActionSelectVariant      = "SELECT_VARIANT"
	ActionRegenerateVariants = "REGENERATE_VARIANTS"
	ActionCancel             = "CANCEL"
)

// Button is an inline action button.
type Button struct {
	Text    string `json:"text"`
	Action  string `json:"action"`
	Payload string `json:"payload"`
}

// Notification is the payload sent to POST /api/notifications.
type Notification struct {
	Channel string   `json:"channel"`
	Title   string   `json:"title"`
	Message string   `json:"message"`
	Buttons []Button `json:"buttons,omitempty"`
}

// Callback is the inbound payload from the telegram-servisi when a user taps a
// button. It mirrors POST /api/telegram/callback.
type Callback struct {
	Action  string `json:"action"`
	Payload string `json:"payload"`
	User    string `json:"user"`
}

// Client sends notifications to the telegram-servisi.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(cfg config.TelegramServiceConfig) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		httpClient: &http.Client{Timeout: cfg.Timeout()},
	}
}

// Send posts a notification. Failures are returned but should not halt the
// pipeline (callers log and continue).
func (c *Client) Send(ctx context.Context, n Notification) error {
	if n.Channel == "" {
		n.Channel = "telegram"
	}
	payload, err := json.Marshal(n)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/notifications", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build notification request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram service returned status %d", resp.StatusCode)
	}
	return nil
}
