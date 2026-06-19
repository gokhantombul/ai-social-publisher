package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"ai-social-publisher/internal/config"
)

func TestUploadAndCleanup(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalStorage(config.StorageConfig{BaseDir: dir}, "https://cdn.example.com/static")
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := t.TempDir()
	source := filepath.Join(sourceDir, "post image.png")
	if err := os.WriteFile(source, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded, err := store.Upload(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.PublicURL != "https://cdn.example.com/static/post%20image.png" {
		t.Fatalf("unexpected public URL: %s", uploaded.PublicURL)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(uploaded.LocalPath, old, old); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Cleanup(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
}
