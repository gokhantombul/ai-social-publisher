package ai

import (
	"context"
	"errors"
	"log/slog"
)

// ErrAllProvidersFailed is returned when every provider in the chain is either
// unavailable or fails. Callers should move the related job to WAITING_AI.
var ErrAllProvidersFailed = errors.New("all AI providers failed")

// modeled is optionally implemented by providers to report their model name.
type modeled interface{ Model() string }

// ScoreResult bundles a score with the provider/model that produced it.
type ScoreResult struct {
	Score    *NewsScore
	Provider string
	Model    string
}

// VariantsResult bundles variants with the provider/model that produced them.
type VariantsResult struct {
	Variants []PostVariant
	Provider string
	Model    string
}

// Service runs the ordered provider chain with graceful fallback.
type Service struct {
	providers []AIProvider
	logger    *slog.Logger
}

// NewService builds the chain. Providers are tried in slice order.
func NewService(logger *slog.Logger, providers ...AIProvider) *Service {
	return &Service{providers: providers, logger: logger.With("component", "ai")}
}

// ScoreNews tries each available provider in order. The first success wins.
func (s *Service) ScoreNews(ctx context.Context, news NewsCandidate) (*ScoreResult, error) {
	for _, p := range s.providers {
		if !p.IsAvailable(ctx) {
			s.logger.Debug("provider unavailable, skipping", "provider", p.Name())
			continue
		}
		score, err := p.ScoreNews(ctx, news)
		if err != nil {
			s.logger.Warn("provider scoring failed, trying next", "provider", p.Name(), "error", err)
			continue
		}
		return &ScoreResult{Score: score, Provider: p.Name(), Model: modelOf(p)}, nil
	}
	return nil, ErrAllProvidersFailed
}

// GeneratePostVariants tries each available provider in order.
func (s *Service) GeneratePostVariants(ctx context.Context, req GeneratePostVariantsRequest) (*VariantsResult, error) {
	for _, p := range s.providers {
		if !p.IsAvailable(ctx) {
			continue
		}
		variants, err := p.GeneratePostVariants(ctx, req)
		if err != nil {
			s.logger.Warn("provider variant generation failed, trying next", "provider", p.Name(), "error", err)
			continue
		}
		if len(variants) == 0 {
			s.logger.Warn("provider returned zero variants, trying next", "provider", p.Name())
			continue
		}
		return &VariantsResult{Variants: variants, Provider: p.Name(), Model: modelOf(p)}, nil
	}
	return nil, ErrAllProvidersFailed
}

// GenerateImagePrompt tries each available provider; falls back to a generated
// prompt from the headline if all providers fail (image prompt is non-critical).
func (s *Service) GenerateImagePrompt(ctx context.Context, news NewsCandidate) (string, error) {
	for _, p := range s.providers {
		if !p.IsAvailable(ctx) {
			continue
		}
		prompt, err := p.GenerateImagePrompt(ctx, news)
		if err != nil || prompt == "" {
			continue
		}
		return prompt, nil
	}
	return "", ErrAllProvidersFailed
}

func modelOf(p AIProvider) string {
	if m, ok := p.(modeled); ok {
		return m.Model()
	}
	return p.Name()
}
