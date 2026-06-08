package news

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"ai-social-publisher/internal/config"
)

// Item is one news item as returned by the external news-service.
type Item struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Summary     string    `json:"summary"`
	Source      string    `json:"source"`
	URL         string    `json:"url"`
	Category    string    `json:"category"`
	PublishedAt time.Time `json:"publishedAt"`
}

type listResponse struct {
	Items []Item `json:"items"`
}

// Client talks to the external haber-servisi.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient constructs a news-service client.
func NewClient(cfg config.NewsServiceConfig) *Client {
	return &Client{
		baseURL:    cfg.BaseURL,
		httpClient: &http.Client{Timeout: cfg.Timeout()},
	}
}

// FetchByCategory pulls news for a single category from GET /api/news?category=.
func (c *Client) FetchByCategory(ctx context.Context, category string) ([]Item, error) {
	endpoint, err := url.Parse(c.baseURL + "/api/news")
	if err != nil {
		return nil, fmt.Errorf("parse news url: %w", err)
	}
	q := endpoint.Query()
	q.Set("category", category)
	endpoint.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build news request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("news request (%s): %w", category, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("news service returned status %d for category %s", resp.StatusCode, category)
	}

	var out listResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode news response: %w", err)
	}
	return out.Items, nil
}
