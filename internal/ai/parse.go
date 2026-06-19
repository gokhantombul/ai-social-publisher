package ai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// codeBlockRe matches a ```json ... ``` or ``` ... ``` fenced block.
var codeBlockRe = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// cleanJSON attempts to recover a JSON document from a raw model response which
// may contain markdown fences or leading/trailing prose.
func cleanJSON(raw string) string {
	s := strings.TrimSpace(raw)

	// If wrapped in a fenced code block, take its contents.
	if m := codeBlockRe.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}

	// Otherwise slice from the first opening brace/bracket to the matching last one.
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		if i := strings.IndexAny(s, "{["); i >= 0 {
			s = s[i:]
		}
	}
	if j := strings.LastIndexAny(s, "}]"); j >= 0 {
		s = s[:j+1]
	}
	return strings.TrimSpace(s)
}

// parseScore parses a scoring response into a validated NewsScore.
func parseScore(raw string) (*NewsScore, error) {
	cleaned := cleanJSON(raw)
	if cleaned == "" {
		return nil, fmt.Errorf("empty AI response")
	}

	var score NewsScore
	if err := json.Unmarshal([]byte(cleaned), &score); err != nil {
		return nil, fmt.Errorf("parse score JSON: %w", err)
	}

	score.ImportanceScore = clamp(score.ImportanceScore, 0, 100)
	score.ViralityScore = clamp(score.ViralityScore, 0, 100)
	if score.AccountFit == "" {
		score.AccountFit = "skip"
	}
	if !oneOf(score.AccountFit, "technology", "cinema", "news", "economy", "skip") {
		return nil, fmt.Errorf("invalid accountFit %q", score.AccountFit)
	}
	if score.RiskLevel == "" {
		score.RiskLevel = "low"
	}
	if !oneOf(score.RiskLevel, "low", "medium", "high") {
		return nil, fmt.Errorf("invalid riskLevel %q", score.RiskLevel)
	}
	if len([]rune(score.Reason)) > 1000 {
		score.Reason = string([]rune(score.Reason)[:1000])
	}
	return &score, nil
}

type variantsEnvelope struct {
	Variants []PostVariant `json:"variants"`
}

// parseVariants parses a variants response. It accepts both the documented
// {"variants":[...]} envelope and a bare [...] array.
func parseVariants(raw string) ([]PostVariant, error) {
	cleaned := cleanJSON(raw)
	if cleaned == "" {
		return nil, fmt.Errorf("empty AI response")
	}

	var env variantsEnvelope
	if err := json.Unmarshal([]byte(cleaned), &env); err == nil && len(env.Variants) > 0 {
		return normalizeVariants(env.Variants), nil
	}

	var arr []PostVariant
	if err := json.Unmarshal([]byte(cleaned), &arr); err == nil && len(arr) > 0 {
		return normalizeVariants(arr), nil
	}

	return nil, fmt.Errorf("parse variants JSON: no variants found")
}

func normalizeVariants(in []PostVariant) []PostVariant {
	out := make([]PostVariant, 0, len(in))
	for _, v := range in {
		if strings.TrimSpace(v.Caption) == "" {
			continue
		}
		if len([]rune(v.Caption)) > 2200 {
			continue
		}
		v.VariantNo = len(out) + 1
		out = append(out, v)
	}
	return out
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
