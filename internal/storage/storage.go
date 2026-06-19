// Package storage abstracts where rendered media is stored and how its public
// URL is derived. The first implementation writes to local disk.
package storage

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai-social-publisher/internal/config"
)

// UploadedFile describes a stored file and its public URL.
type UploadedFile struct {
	// PublicURL is an absolute URL Instagram can fetch.
	PublicURL string
	// LocalPath is where the file lives on disk (local driver).
	LocalPath string
}

// Storage stores a local file and returns its publicly reachable URL.
type Storage interface {
	Upload(ctx context.Context, filePath string) (*UploadedFile, error)
}

// LocalStorage copies files into a base directory and builds public URLs by
// joining the configured public base URL with the file name.
type LocalStorage struct {
	baseDir       string
	publicBaseURL string
}

// NewLocalStorage creates the base directory if needed.
func NewLocalStorage(cfg config.StorageConfig, publicBaseURL string) (*LocalStorage, error) {
	if err := os.MkdirAll(cfg.BaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage dir: %w", err)
	}
	return &LocalStorage{
		baseDir:       cfg.BaseDir,
		publicBaseURL: strings.TrimRight(publicBaseURL, "/"),
	}, nil
}

// BaseDir exposes the storage directory so the HTTP server can serve it.
func (s *LocalStorage) BaseDir() string { return s.baseDir }

// Upload copies filePath into the base directory (if not already there) and
// returns its public URL. If the source is already inside baseDir it is kept.
func (s *LocalStorage) Upload(ctx context.Context, filePath string) (*UploadedFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat upload source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("upload source is not a regular file")
	}
	name := filepath.Base(filePath)
	dest := filepath.Join(s.baseDir, name)

	abs, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("resolve upload source: %w", err)
	}
	destAbs, err := filepath.Abs(dest)
	if err != nil {
		return nil, fmt.Errorf("resolve upload destination: %w", err)
	}
	if abs != destAbs {
		if err := copyFile(filePath, dest); err != nil {
			return nil, err
		}
	}

	return &UploadedFile{
		PublicURL: s.publicBaseURL + "/" + url.PathEscape(name),
		LocalPath: dest,
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	out, err := os.CreateTemp(filepath.Dir(dst), ".upload-*")
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	tmpName := out.Name()
	defer os.Remove(tmpName) //nolint:errcheck

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return fmt.Errorf("copy file: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("sync dest: %w", err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close dest: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod dest: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("commit dest: %w", err)
	}
	return nil
}

// Cleanup removes generated regular files older than cutoff. Symlinks and
// directories are never followed or removed.
func (s *LocalStorage) Cleanup(ctx context.Context, cutoff time.Time) (int, error) {
	entries, err := os.ReadDir(s.baseDir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(s.baseDir, entry.Name())); err != nil {
			return removed, fmt.Errorf("remove expired media %q: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}
