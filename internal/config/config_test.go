package config

import (
	"os"
	"path/filepath"
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
database:
  url: "${DB_URL}"
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
database:
  url: "postgres://localhost/test"
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
