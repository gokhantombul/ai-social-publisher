// Package database manages the PostgreSQL connection pool and schema migrations.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"ai-social-publisher/internal/config"
	"ai-social-publisher/migrations"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx database/sql driver
	"github.com/pressly/goose/v3"
)

// Connect opens a database/sql pool backed by the pgx stdlib driver and verifies
// connectivity with a ping.
func Connect(ctx context.Context, cfg config.DatabaseConfig) (*sql.DB, error) {
	db, err := sql.Open("pgx", cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime())

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return db, nil
}

// Migrate applies all pending goose migrations from the embedded migrations FS.
func Migrate(db *sql.DB, logger *slog.Logger) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{logger})

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	if err := goose.Up(db, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back a single migration. Used by the `migrate down` command.
func MigrateDown(db *sql.DB, logger *slog.Logger) error {
	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	return goose.Down(db, ".")
}

// gooseLogger adapts slog to goose's logger interface.
type gooseLogger struct{ l *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...interface{}) {
	g.l.Error(fmt.Sprintf(format, v...))
}

func (g gooseLogger) Printf(format string, v ...interface{}) {
	g.l.Info(fmt.Sprintf(format, v...))
}
