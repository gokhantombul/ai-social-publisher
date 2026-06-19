package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"ai-social-publisher/internal/config"
)

// OllamaProvider is the fallback AI provider using the Ollama HTTP API.
type OllamaProvider struct {
	cfg    config.OllamaConfig
	client *http.Client
	logger *slog.Logger
}

// NewOllamaProvider constructs the Ollama-backed provider.
func NewOllamaProvider(cfg config.OllamaConfig, logger *slog.Logger) *OllamaProvider {
	return &OllamaProvider{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout()},
		logger: logger.With("provider", "ollama"),
	}
}

func (p *OllamaProvider) Name() string { return "ollama" }

// Model reports the configured Ollama model name.
func (p *OllamaProvider) Model() string { return p.cfg.Model }

// IsAvailable hits /api/tags as a lightweight health check.
func (p *OllamaProvider) IsAvailable(ctx context.Context) bool {
	if !p.cfg.Enabled {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.BaseURL+"/api/tags", nil)
	if err != nil {
		return false
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (p *OllamaProvider) ScoreNews(ctx context.Context, news NewsCandidate) (*NewsScore, error) {
	out, err := p.generate(ctx, buildScorePrompt(news), true)
	if err != nil {
		return nil, err
	}
	return parseScore(out)
}

func (p *OllamaProvider) GeneratePostVariants(ctx context.Context, req GeneratePostVariantsRequest) ([]PostVariant, error) {
	out, err := p.generate(ctx, buildVariantsPrompt(req), true)
	if err != nil {
		return nil, err
	}
	return parseVariants(out)
}

type ollamaGenerateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	Format string `json:"format,omitempty"`
}

type ollamaGenerateResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
}

// generate calls /api/generate (non-streaming). When jsonMode is true it asks
// Ollama to constrain output to JSON via the "format" field.
func (p *OllamaProvider) generate(ctx context.Context, prompt string, jsonMode bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()

	body := ollamaGenerateRequest{
		Model:  p.cfg.Model,
		Prompt: prompt,
		Stream: false,
	}
	if jsonMode {
		body.Format = "json"
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("marshal ollama request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.BaseURL+"/api/generate", bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("build ollama request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("ollama timed out after %s", p.cfg.Timeout())
		}
		return "", fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama returned status %d", resp.StatusCode)
	}

	var out ollamaGenerateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("decode ollama response: %w", err)
	}

	if strings.TrimSpace(out.Response) == "" {
		return "", fmt.Errorf("ollama returned empty response")
	}
	return out.Response, nil
}
