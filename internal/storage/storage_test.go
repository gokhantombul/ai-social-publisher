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

func TestCleanupRemovesOrphanedTempFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := NewLocalStorage(config.StorageConfig{BaseDir: dir}, "https://cdn.example.com/static")
	if err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, ".upload-orphan")
	fresh := filepath.Join(dir, ".upload-fresh")
	dotfile := filepath.Join(dir, ".gitkeep")
	for _, p := range []string{orphan, fresh, dotfile} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphan, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dotfile, stale, stale); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Cleanup(context.Background(), time.Now().Add(-24*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("stale temp file should be removed")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh temp file must be kept (copy may be in progress)")
	}
	if _, err := os.Stat(dotfile); err != nil {
		t.Error("unrelated dotfiles must never be removed")
	}
}
