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
	unlock, err := migrationLock(db)
	if err != nil {
		return err
	}
	defer unlock()

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
	unlock, err := migrationLock(db)
	if err != nil {
		return err
	}
	defer unlock()

	goose.SetBaseFS(migrations.FS)
	goose.SetLogger(gooseLogger{logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	return goose.Down(db, ".")
}

// migrationLock serializes migrations across concurrently starting application
// instances. The advisory lock is held by a dedicated connection while Goose
// uses the pool to execute migration statements.
func migrationLock(db *sql.DB) (func(), error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire migration connection: %w", err)
	}
	const lockID int64 = 0x41495350 // "AISP"
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	return func() {
		_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)
		_ = conn.Close()
	}, nil
}

// gooseLogger adapts slog to goose's logger interface.
type gooseLogger struct{ l *slog.Logger }

func (g gooseLogger) Fatalf(format string, v ...interface{}) {
	g.l.Error(fmt.Sprintf(format, v...))
}

func (g gooseLogger) Printf(format string, v ...interface{}) {
	g.l.Info(fmt.Sprintf(format, v...))
}
