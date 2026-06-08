// Package storage abstracts where rendered media is stored and how its public
// URL is derived. The first implementation writes to local disk.
package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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
	name := filepath.Base(filePath)
	dest := filepath.Join(s.baseDir, name)

	if abs, err := filepath.Abs(filePath); err == nil {
		if destAbs, derr := filepath.Abs(dest); derr == nil && abs != destAbs {
			if err := copyFile(filePath, dest); err != nil {
				return nil, err
			}
		}
	}

	return &UploadedFile{
		PublicURL: s.publicBaseURL + "/" + name,
		LocalPath: dest,
	}, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create dest: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy file: %w", err)
	}
	return nil
}
