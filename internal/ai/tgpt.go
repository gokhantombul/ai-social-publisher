package ai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"ai-social-publisher/internal/config"
)

// TgptProvider is the primary AI provider. It shells out to the `tgpt` CLI.
type TgptProvider struct {
	cfg    config.TgptConfig
	logger *slog.Logger
}

// NewTgptProvider constructs the tgpt-backed provider.
func NewTgptProvider(cfg config.TgptConfig, logger *slog.Logger) *TgptProvider {
	return &TgptProvider{cfg: cfg, logger: logger.With("provider", "tgpt")}
}

func (p *TgptProvider) Name() string { return "tgpt" }

// Model reports the underlying command (tgpt picks its own backend model).
func (p *TgptProvider) Model() string { return p.cfg.Command }

// IsAvailable reports whether tgpt is enabled in config and resolvable on PATH.
func (p *TgptProvider) IsAvailable(ctx context.Context) bool {
	if !p.cfg.Enabled {
		return false
	}
	if _, err := exec.LookPath(p.cfg.Command); err != nil {
		return false
	}
	return true
}

func (p *TgptProvider) ScoreNews(ctx context.Context, news NewsCandidate) (*NewsScore, error) {
	out, err := p.run(ctx, buildScorePrompt(news))
	if err != nil {
		return nil, err
	}
	return parseScore(out)
}

func (p *TgptProvider) GeneratePostVariants(ctx context.Context, req GeneratePostVariantsRequest) ([]PostVariant, error) {
	out, err := p.run(ctx, buildVariantsPrompt(req))
	if err != nil {
		return nil, err
	}
	return parseVariants(out)
}

// run executes the tgpt command with the prompt as a single argument, enforcing
// the configured timeout. Note: the prompt is logged at debug level but tokens
// are never part of the prompt, so nothing sensitive is logged.
func (p *TgptProvider) run(ctx context.Context, prompt string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout())
	defer cancel()

	cmd := exec.CommandContext(ctx, p.cfg.Command, prompt)
	cmd.Env = allowedEnvironment(p.cfg.AllowedEnv)

	stdout := newLimitedBuffer(2 << 20)
	stderr := newLimitedBuffer(64 << 10)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("tgpt timed out after %s", p.cfg.Timeout())
		}
		p.logger.Warn("tgpt command failed", "error", err, "stderr", truncate(stderr.String(), 500))
		return "", fmt.Errorf("tgpt run: %w", err)
	}

	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return "", fmt.Errorf("tgpt returned empty output")
	}
	return out, nil
}

type limitedBuffer struct {
	buf       bytes.Buffer
	remaining int
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{remaining: limit} }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	if len(p) > 0 {
		_, _ = b.buf.Write(p)
		b.remaining -= len(p)
	}
	return original, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func allowedEnvironment(names []string) []string {
	env := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
