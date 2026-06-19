package instagram

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"ai-social-publisher/internal/config"
)

func TestPublishImageTwoStepFlow(t *testing.T) {
	publisher := NewPublisher(config.InstagramConfig{
		GraphBaseURL: "https://graph.example.com", APIVersion: "v1", AccessToken: "secret-token", PublishEnabled: true,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	publisher.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.ParseForm(); err != nil || r.Form.Get("access_token") != "secret-token" {
			t.Fatalf("bad form")
		}
		response := ""
		switch r.URL.Path {
		case "/v1/user-1/media":
			if r.Form.Get("image_url") == "" || r.Form.Get("caption") == "" {
				t.Fatalf("missing media fields")
			}
			response = `{"id":"creation-1"}`
		case "/v1/user-1/media_publish":
			if r.Form.Get("creation_id") != "creation-1" {
				t.Fatalf("bad creation")
			}
			response = `{"id":"media-1"}`
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response)), Header: make(http.Header)}, nil
	})
	result, err := publisher.PublishImage(context.Background(), PublishRequest{
		InstagramUserID: "user-1", ImageURL: "https://cdn.example.com/image.png", Caption: "caption",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CreationID != "creation-1" || result.MediaID != "media-1" || result.DryRun {
		t.Fatalf("unexpected result: %+v", result)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
