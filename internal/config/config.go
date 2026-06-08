// Package config loads application configuration from a YAML file with
// ${ENV_VAR} expansion, applies sensible defaults and validates the result.
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration object.
type Config struct {
	App             AppConfig             `yaml:"app"`
	Database        DatabaseConfig        `yaml:"database"`
	NewsService     NewsServiceConfig     `yaml:"news_service"`
	TelegramService TelegramServiceConfig `yaml:"telegram_service"`
	AI              AIConfig              `yaml:"ai"`
	PostGeneration  PostGenerationConfig  `yaml:"post_generation"`
	Instagram       InstagramConfig       `yaml:"instagram"`
	Storage         StorageConfig         `yaml:"storage"`
	Scheduler       SchedulerConfig       `yaml:"scheduler"`
	Accounts        []AccountConfig       `yaml:"accounts"`
}

type AppConfig struct {
	Env           string `yaml:"env"`
	HTTPPort      int    `yaml:"http_port"`
	PublicBaseURL string `yaml:"public_base_url"`
}

type DatabaseConfig struct {
	URL                    string `yaml:"url"`
	MaxOpenConns           int    `yaml:"max_open_conns"`
	MaxIdleConns           int    `yaml:"max_idle_conns"`
	ConnMaxLifetimeMinutes int    `yaml:"conn_max_lifetime_minutes"`
	AutoMigrate            bool   `yaml:"auto_migrate"`
}

func (d DatabaseConfig) ConnMaxLifetime() time.Duration {
	return time.Duration(d.ConnMaxLifetimeMinutes) * time.Minute
}

type NewsServiceConfig struct {
	BaseURL        string   `yaml:"base_url"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Categories     []string `yaml:"categories"`
}

func (n NewsServiceConfig) Timeout() time.Duration {
	return time.Duration(n.TimeoutSeconds) * time.Second
}

type TelegramServiceConfig struct {
	BaseURL        string `yaml:"base_url"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

func (t TelegramServiceConfig) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

type AIConfig struct {
	Providers AIProvidersConfig `yaml:"providers"`
}

type AIProvidersConfig struct {
	Tgpt   TgptConfig   `yaml:"tgpt"`
	Ollama OllamaConfig `yaml:"ollama"`
}

type TgptConfig struct {
	Enabled        bool   `yaml:"enabled"`
	Command        string `yaml:"command"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

func (t TgptConfig) Timeout() time.Duration {
	return time.Duration(t.TimeoutSeconds) * time.Second
}

type OllamaConfig struct {
	Enabled        bool   `yaml:"enabled"`
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

func (o OllamaConfig) Timeout() time.Duration {
	return time.Duration(o.TimeoutSeconds) * time.Second
}

type PostGenerationConfig struct {
	DefaultVariantCount int `yaml:"default_variant_count"`
	MaxVariantCount     int `yaml:"max_variant_count"`
}

type InstagramConfig struct {
	GraphBaseURL   string `yaml:"graph_base_url"`
	APIVersion     string `yaml:"api_version"`
	AccessToken    string `yaml:"access_token"`
	PublishEnabled bool   `yaml:"publish_enabled"`
}

type StorageConfig struct {
	Driver  string `yaml:"driver"`
	BaseDir string `yaml:"base_dir"`
}

type SchedulerConfig struct {
	Enabled                       bool `yaml:"enabled"`
	NewsSyncIntervalMinutes       int  `yaml:"news_sync_interval_minutes"`
	WaitingAIRetryIntervalMinutes int  `yaml:"waiting_ai_retry_interval_minutes"`
	PublishIntervalMinutes        int  `yaml:"publish_interval_minutes"`
}

// AccountConfig is the per-channel configuration read from YAML. It is synced
// into the social_accounts table on startup.
type AccountConfig struct {
	Code            string   `yaml:"code"`
	Name            string   `yaml:"name"`
	Category        string   `yaml:"category"`
	InstagramUserID string   `yaml:"instagram_user_id"`
	VariantCount    int      `yaml:"variant_count"`
	NotifyThreshold int      `yaml:"notify_threshold"`
	AutoPublish     bool     `yaml:"auto_publish"`
	Styles          []string `yaml:"styles"`
}

// Load reads the YAML config file at path, expands ${ENV} placeholders, applies
// defaults and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	// Expand ${VAR} and $VAR using the current environment.
	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.App.HTTPPort == 0 {
		c.App.HTTPPort = 8080
	}
	if c.App.Env == "" {
		c.App.Env = "development"
	}
	if c.Database.MaxOpenConns == 0 {
		c.Database.MaxOpenConns = 10
	}
	if c.Database.MaxIdleConns == 0 {
		c.Database.MaxIdleConns = 5
	}
	if c.Database.ConnMaxLifetimeMinutes == 0 {
		c.Database.ConnMaxLifetimeMinutes = 30
	}
	if c.NewsService.TimeoutSeconds == 0 {
		c.NewsService.TimeoutSeconds = 20
	}
	if len(c.NewsService.Categories) == 0 {
		c.NewsService.Categories = []string{"technology", "cinema", "news", "economy"}
	}
	if c.TelegramService.TimeoutSeconds == 0 {
		c.TelegramService.TimeoutSeconds = 15
	}
	if c.AI.Providers.Tgpt.Command == "" {
		c.AI.Providers.Tgpt.Command = "tgpt"
	}
	if c.AI.Providers.Tgpt.TimeoutSeconds == 0 {
		c.AI.Providers.Tgpt.TimeoutSeconds = 120
	}
	if c.AI.Providers.Ollama.BaseURL == "" {
		c.AI.Providers.Ollama.BaseURL = "http://localhost:11434"
	}
	if c.AI.Providers.Ollama.Model == "" {
		c.AI.Providers.Ollama.Model = "llama3.1:8b"
	}
	if c.AI.Providers.Ollama.TimeoutSeconds == 0 {
		c.AI.Providers.Ollama.TimeoutSeconds = 90
	}
	if c.PostGeneration.DefaultVariantCount == 0 {
		c.PostGeneration.DefaultVariantCount = 3
	}
	if c.PostGeneration.MaxVariantCount == 0 {
		c.PostGeneration.MaxVariantCount = 10
	}
	if c.Instagram.GraphBaseURL == "" {
		c.Instagram.GraphBaseURL = "https://graph.facebook.com"
	}
	if c.Instagram.APIVersion == "" {
		c.Instagram.APIVersion = "v23.0"
	}
	if c.Storage.Driver == "" {
		c.Storage.Driver = "local"
	}
	if c.Storage.BaseDir == "" {
		c.Storage.BaseDir = "./storage/uploads"
	}
	if c.Scheduler.NewsSyncIntervalMinutes == 0 {
		c.Scheduler.NewsSyncIntervalMinutes = 10
	}
	if c.Scheduler.WaitingAIRetryIntervalMinutes == 0 {
		c.Scheduler.WaitingAIRetryIntervalMinutes = 15
	}
	if c.Scheduler.PublishIntervalMinutes == 0 {
		c.Scheduler.PublishIntervalMinutes = 1
	}

	// Clamp / default per-account variant counts.
	for i := range c.Accounts {
		a := &c.Accounts[i]
		if a.VariantCount <= 0 {
			a.VariantCount = c.PostGeneration.DefaultVariantCount
		}
		if a.VariantCount > c.PostGeneration.MaxVariantCount {
			a.VariantCount = c.PostGeneration.MaxVariantCount
		}
		if a.NotifyThreshold == 0 {
			a.NotifyThreshold = 80
		}
	}
}

func (c *Config) validate() error {
	if c.Database.URL == "" {
		return fmt.Errorf("database.url is required")
	}
	if len(c.Accounts) == 0 {
		return fmt.Errorf("at least one account must be configured")
	}
	seen := map[string]bool{}
	for _, a := range c.Accounts {
		if a.Code == "" {
			return fmt.Errorf("account code must not be empty")
		}
		if seen[a.Code] {
			return fmt.Errorf("duplicate account code %q", a.Code)
		}
		seen[a.Code] = true
		if a.Category == "" {
			return fmt.Errorf("account %q: category is required", a.Code)
		}
	}
	return nil
}

// AccountByCategory returns the configured account for a news category.
func (c *Config) AccountByCategory(category string) (AccountConfig, bool) {
	for _, a := range c.Accounts {
		if a.Category == category {
			return a, true
		}
	}
	return AccountConfig{}, false
}
