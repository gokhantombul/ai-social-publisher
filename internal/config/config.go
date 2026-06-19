// Package config loads application configuration from a YAML file with
// ${ENV_VAR} expansion, applies sensible defaults and validates the result.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
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
	Security        SecurityConfig        `yaml:"security"`
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
	AuthToken      string   `yaml:"auth_token"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	Categories     []string `yaml:"categories"`
}

func (n NewsServiceConfig) Timeout() time.Duration {
	return time.Duration(n.TimeoutSeconds) * time.Second
}

type TelegramServiceConfig struct {
	BaseURL        string `yaml:"base_url"`
	AuthToken      string `yaml:"auth_token"`
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
	Enabled        bool     `yaml:"enabled"`
	Command        string   `yaml:"command"`
	TimeoutSeconds int      `yaml:"timeout_seconds"`
	AllowedEnv     []string `yaml:"allowed_env"`
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
	Driver        string `yaml:"driver"`
	BaseDir       string `yaml:"base_dir"`
	RetentionDays int    `yaml:"retention_days"`
}

type SchedulerConfig struct {
	Enabled                       bool `yaml:"enabled"`
	NewsSyncIntervalMinutes       int  `yaml:"news_sync_interval_minutes"`
	WaitingAIRetryIntervalMinutes int  `yaml:"waiting_ai_retry_interval_minutes"`
	PublishIntervalMinutes        int  `yaml:"publish_interval_minutes"`
	WorkIntervalSeconds           int  `yaml:"work_interval_seconds"`
	NotificationIntervalSeconds   int  `yaml:"notification_interval_seconds"`
	StaleJobTimeoutMinutes        int  `yaml:"stale_job_timeout_minutes"`
}

// SecurityConfig protects the administrative API and authenticates callbacks
// sent by telegram-service. Secrets must be supplied through environment-backed
// config values and are never logged.
type SecurityConfig struct {
	APIToken               string   `yaml:"api_token"`
	TelegramCallbackSecret string   `yaml:"telegram_callback_secret"`
	AllowedTelegramUsers   []string `yaml:"allowed_telegram_users"`
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
	Styles          []string `yaml:"styles"`
}

// Load reads the YAML config file at path, expands ${ENV} placeholders, applies
// defaults and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", path, err)
	}

	// Expand ${VAR} and $VAR, but fail closed when a referenced variable is
	// missing. os.ExpandEnv silently replaces missing values with an empty
	// string, which can otherwise turn a typo into an unsafe configuration.
	missing := map[string]struct{}{}
	expanded := os.Expand(string(raw), func(key string) string {
		value, ok := os.LookupEnv(key)
		if !ok {
			missing[key] = struct{}{}
		}
		return value
	})
	if len(missing) > 0 {
		keys := make([]string, 0, len(missing))
		for key := range missing {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return nil, fmt.Errorf("missing environment variables: %s", strings.Join(keys, ", "))
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewBufferString(expanded))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
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
	if len(c.AI.Providers.Tgpt.AllowedEnv) == 0 {
		c.AI.Providers.Tgpt.AllowedEnv = []string{"PATH", "HOME", "TMPDIR", "LANG", "SSL_CERT_FILE"}
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
	if c.Storage.RetentionDays == 0 {
		c.Storage.RetentionDays = 30
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
	if c.Scheduler.WorkIntervalSeconds == 0 {
		c.Scheduler.WorkIntervalSeconds = 5
	}
	if c.Scheduler.NotificationIntervalSeconds == 0 {
		c.Scheduler.NotificationIntervalSeconds = 5
	}
	if c.Scheduler.StaleJobTimeoutMinutes == 0 {
		c.Scheduler.StaleJobTimeoutMinutes = 10
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
	databaseURL, err := url.Parse(c.Database.URL)
	if err != nil || databaseURL.Host == "" || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		return fmt.Errorf("database.url must be a valid postgres URL")
	}
	if c.App.Env == "production" && strings.EqualFold(databaseURL.Query().Get("sslmode"), "disable") {
		return fmt.Errorf("database.url must not disable TLS in production")
	}
	if len(c.Accounts) == 0 {
		return fmt.Errorf("at least one account must be configured")
	}
	if c.App.HTTPPort < 1 || c.App.HTTPPort > 65535 {
		return fmt.Errorf("app.http_port must be between 1 and 65535")
	}
	if !oneOfString(c.App.Env, "development", "test", "production") {
		return fmt.Errorf("app.env must be development, test or production")
	}
	if c.Database.MaxOpenConns < 1 || c.Database.MaxIdleConns < 0 || c.Database.MaxIdleConns > c.Database.MaxOpenConns {
		return fmt.Errorf("database connection pool limits are invalid")
	}
	if c.NewsService.TimeoutSeconds < 1 || c.TelegramService.TimeoutSeconds < 1 || c.AI.Providers.Tgpt.TimeoutSeconds < 1 || c.AI.Providers.Ollama.TimeoutSeconds < 1 {
		return fmt.Errorf("service timeout values must be positive")
	}
	if c.Scheduler.NewsSyncIntervalMinutes < 1 || c.Scheduler.WaitingAIRetryIntervalMinutes < 1 || c.Scheduler.PublishIntervalMinutes < 1 ||
		c.Scheduler.WorkIntervalSeconds < 1 || c.Scheduler.NotificationIntervalSeconds < 1 || c.Scheduler.StaleJobTimeoutMinutes < 1 {
		return fmt.Errorf("scheduler intervals must be positive")
	}
	requireServiceHTTPS := c.App.Env == "production"
	if err := validateHTTPURL("news_service.base_url", c.NewsService.BaseURL, requireServiceHTTPS); err != nil {
		return err
	}
	if err := validateHTTPURL("telegram_service.base_url", c.TelegramService.BaseURL, requireServiceHTTPS); err != nil {
		return err
	}
	if len(c.NewsService.AuthToken) < 32 || len(c.TelegramService.AuthToken) < 32 {
		return fmt.Errorf("news_service.auth_token and telegram_service.auth_token must be at least 32 characters")
	}
	if err := validateHTTPURL("instagram.graph_base_url", c.Instagram.GraphBaseURL, true); err != nil {
		return err
	}
	if c.Storage.Driver != "local" {
		return fmt.Errorf("storage.driver %q is unsupported (supported: local)", c.Storage.Driver)
	}
	if c.Storage.RetentionDays < 1 {
		return fmt.Errorf("storage.retention_days must be positive")
	}
	if c.App.PublicBaseURL == "" {
		return fmt.Errorf("app.public_base_url is required")
	}
	if err := validateHTTPURL("app.public_base_url", c.App.PublicBaseURL, c.Instagram.PublishEnabled); err != nil {
		return err
	}
	if c.Instagram.PublishEnabled {
		publicURL, _ := url.Parse(c.App.PublicBaseURL)
		host := strings.ToLower(publicURL.Hostname())
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return fmt.Errorf("app.public_base_url must be publicly reachable when Instagram publishing is enabled")
		}
		if strings.TrimSpace(c.Instagram.AccessToken) == "" {
			return fmt.Errorf("instagram.access_token is required when publishing is enabled")
		}
	}
	if len(c.Security.APIToken) < 32 {
		return fmt.Errorf("security.api_token must be at least 32 characters")
	}
	if len(c.Security.TelegramCallbackSecret) < 32 {
		return fmt.Errorf("security.telegram_callback_secret must be at least 32 characters")
	}
	if len(c.Security.AllowedTelegramUsers) == 0 {
		return fmt.Errorf("security.allowed_telegram_users must contain at least one user")
	}
	if !c.AI.Providers.Tgpt.Enabled && !c.AI.Providers.Ollama.Enabled {
		return fmt.Errorf("at least one AI provider must be enabled")
	}
	if c.AI.Providers.Ollama.Enabled {
		if err := validateHTTPURL("ai.providers.ollama.base_url", c.AI.Providers.Ollama.BaseURL, false); err != nil {
			return err
		}
		if strings.TrimSpace(c.AI.Providers.Ollama.Model) == "" {
			return fmt.Errorf("ai.providers.ollama.model is required when enabled")
		}
	}
	if c.AI.Providers.Tgpt.Enabled && strings.TrimSpace(c.AI.Providers.Tgpt.Command) == "" {
		return fmt.Errorf("ai.providers.tgpt.command is required when enabled")
	}
	for _, name := range c.AI.Providers.Tgpt.AllowedEnv {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("ai.providers.tgpt.allowed_env contains an invalid variable name")
		}
	}
	if c.PostGeneration.MaxVariantCount < 1 || c.PostGeneration.MaxVariantCount > 10 || c.PostGeneration.DefaultVariantCount < 1 || c.PostGeneration.DefaultVariantCount > c.PostGeneration.MaxVariantCount {
		return fmt.Errorf("post_generation variant counts are invalid")
	}
	seen := map[string]bool{}
	seenCategories := map[string]bool{}
	configuredNewsCategories := map[string]bool{}
	for _, category := range c.NewsService.Categories {
		category = strings.TrimSpace(category)
		if category == "" || configuredNewsCategories[category] {
			return fmt.Errorf("news_service.categories contains an empty or duplicate category")
		}
		configuredNewsCategories[category] = true
	}
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
		if seenCategories[a.Category] {
			return fmt.Errorf("duplicate account category %q is unsupported", a.Category)
		}
		seenCategories[a.Category] = true
		if !configuredNewsCategories[a.Category] {
			return fmt.Errorf("account %q category %q is not included in news_service.categories", a.Code, a.Category)
		}
		if a.NotifyThreshold < 0 || a.NotifyThreshold > 100 {
			return fmt.Errorf("account %q: notify_threshold must be between 0 and 100", a.Code)
		}
		if c.Instagram.PublishEnabled && strings.TrimSpace(a.InstagramUserID) == "" {
			return fmt.Errorf("account %q: instagram_user_id is required when publishing is enabled", a.Code)
		}
	}
	for _, user := range c.Security.AllowedTelegramUsers {
		if strings.TrimSpace(user) == "" {
			return fmt.Errorf("security.allowed_telegram_users must not contain empty users")
		}
	}
	return nil
}

func validateHTTPURL(name, raw string, requireHTTPS bool) error {
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s must be a valid absolute HTTP URL", name)
	}
	if requireHTTPS && u.Scheme != "https" {
		return fmt.Errorf("%s must use HTTPS", name)
	}
	return nil
}

func oneOfString(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
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
