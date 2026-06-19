package news

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ai-social-publisher/internal/config"
)

func TestFetchByCategoryAuthenticatesAndEncodesQuery(t *testing.T) {
	client := NewClient(config.NewsServiceConfig{
		BaseURL: "https://news.example.com", AuthToken: "12345678901234567890123456789012", TimeoutSeconds: 2,
	})
	client.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer 12345678901234567890123456789012" {
			t.Fatalf("missing authorization header")
		}
		if r.URL.Query().Get("category") != "technology" {
			t.Fatalf("wrong category query")
		}
		var body strings.Builder
		_ = json.NewEncoder(&body).Encode(map[string]any{"items": []map[string]any{{
			"id": "1", "title": "test", "category": "technology",
		}}})
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body.String())), Header: make(http.Header)}, nil
	})
	items, err := client.FetchByCategory(context.Background(), "technology")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%v err=%v", items, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
