// Package ai implements the AI provider chain (tgpt primary, Ollama fallback)
// used for news scoring, post variant generation and image prompt generation.
package ai

import (
	"context"
	"time"
)

// NewsCandidate is the minimal news view the AI layer needs. It is mapped from
// the news domain model by callers.
type NewsCandidate struct {
	ID          string
	Title       string
	Summary     string
	Source      string
	SourceURL   string
	Category    string
	PublishedAt time.Time
}

// NewsScore is the structured result of scoring a news item.
type NewsScore struct {
	ImportanceScore int    `json:"importanceScore"`
	ViralityScore   int    `json:"viralityScore"`
	AccountFit      string `json:"accountFit"` // technology|cinema|news|economy|skip
	ShouldNotify    bool   `json:"shouldNotify"`
	RiskLevel       string `json:"riskLevel"` // low|medium|high
	Reason          string `json:"reason"`
}

// PostVariant is a single generated caption alternative.
type PostVariant struct {
	VariantNo int    `json:"variantNo"`
	Style     string `json:"style"`
	Caption   string `json:"caption"`
}

// GeneratePostVariantsRequest carries everything needed to build the dynamic
// post-generation prompt.
type GeneratePostVariantsRequest struct {
	News         NewsCandidate
	Category     string
	VariantCount int
	Styles       []string
}

// AIProvider is implemented by each concrete provider (tgpt, Ollama).
type AIProvider interface {
	Name() string
	IsAvailable(ctx context.Context) bool
	ScoreNews(ctx context.Context, news NewsCandidate) (*NewsScore, error)
	GeneratePostVariants(ctx context.Context, req GeneratePostVariantsRequest) ([]PostVariant, error)
	GenerateImagePrompt(ctx context.Context, news NewsCandidate) (string, error)
}
