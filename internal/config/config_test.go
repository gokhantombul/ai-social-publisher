package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultsAndClamp(t *testing.T) {
	t.Setenv("DB_URL", "postgres://localhost/test")
	yaml := `
app:
  public_base_url: "http://localhost:8080/static"
database:
  url: "${DB_URL}"
news_service:
  base_url: "http://localhost:9001"
  auth_token: "12345678901234567890123456789012"
  categories: ["technology", "news"]
telegram_service:
  base_url: "http://localhost:9002"
  auth_token: "12345678901234567890123456789012"
ai:
  providers:
    tgpt:
      enabled: true
security:
  api_token: "12345678901234567890123456789012"
  telegram_callback_secret: "12345678901234567890123456789012"
  allowed_telegram_users: ["tester"]
post_generation:
  default_variant_count: 3
  max_variant_count: 4
accounts:
  - code: "tech"
    category: "technology"
    variant_count: 99
  - code: "news"
    category: "news"
`
	cfg, err := Load(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if cfg.App.HTTPPort != 8080 {
		t.Errorf("default http port not applied: %d", cfg.App.HTTPPort)
	}
	if cfg.Database.URL != "postgres://localhost/test" {
		t.Errorf("env expansion failed: %q", cfg.Database.URL)
	}
	// variant_count 99 should be clamped to max 4.
	if cfg.Accounts[0].VariantCount != 4 {
		t.Errorf("variant count not clamped: %d", cfg.Accounts[0].VariantCount)
	}
	// missing variant_count should fall back to default 3.
	if cfg.Accounts[1].VariantCount != 3 {
		t.Errorf("variant count default not applied: %d", cfg.Accounts[1].VariantCount)
	}
}

func TestValidateRequiresDatabaseURL(t *testing.T) {
	yaml := `
app:
  public_base_url: "http://localhost:8080/static"
news_service:
  base_url: "http://localhost:9001"
  auth_token: "12345678901234567890123456789012"
  categories: ["technology"]
telegram_service:
  base_url: "http://localhost:9002"
  auth_token: "12345678901234567890123456789012"
ai:
  providers:
    tgpt:
      enabled: true
security:
  api_token: "12345678901234567890123456789012"
  telegram_callback_secret: "12345678901234567890123456789012"
  allowed_telegram_users: ["tester"]
accounts:
  - code: "tech"
    category: "technology"
`
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Error("expected error when database.url missing")
	}
}

func TestValidateRejectsDuplicateCodes(t *testing.T) {
	yaml := `
app:
  public_base_url: "http://localhost:8080/static"
database:
  url: "postgres://localhost/test"
news_service:
  base_url: "http://localhost:9001"
  auth_token: "12345678901234567890123456789012"
  categories: ["technology", "news"]
telegram_service:
  base_url: "http://localhost:9002"
  auth_token: "12345678901234567890123456789012"
ai:
  providers:
    tgpt:
      enabled: true
security:
  api_token: "12345678901234567890123456789012"
  telegram_callback_secret: "12345678901234567890123456789012"
  allowed_telegram_users: ["tester"]
accounts:
  - code: "dup"
    category: "technology"
  - code: "dup"
    category: "news"
`
	if _, err := Load(writeTemp(t, yaml)); err == nil {
		t.Error("expected error for duplicate account codes")
	}
}

func TestLoadRejectsUnknownFieldsAndMissingEnv(t *testing.T) {
	path := writeTemp(t, `database:
  url: "${MISSING_DATABASE_URL}"
unknown_section: true
`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected missing environment variable error")
	}

	t.Setenv("MISSING_DATABASE_URL", "postgres://localhost/test")
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown YAML field error")
	}
}

func TestExampleConfigOnlyReferencesDocumentedEnvironmentVariables(t *testing.T) {
	configRaw, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	envRaw, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)\}`).FindAllStringSubmatch(string(configRaw), -1) {
		if !strings.Contains("\n"+string(envRaw), "\n"+match[1]+"=") {
			t.Errorf("config.example.yaml references undocumented environment variable %q", match[1])
		}
	}
}
