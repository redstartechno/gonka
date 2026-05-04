package storage

import (
	"context"
	"log/slog"
	"os"
)

// NewStorage builds the canonical Storage for a host process.
//
//   - If PGHOST is set and the connection succeeds, use Postgres.
//   - Otherwise fall back to SQLite under sqliteDir for the lifetime of this
//     process (no mid-flight reconnect attempts).
//
// This mirrors the simpler-than-HybridStorage design the host wants: pick one
// backend at boot, log it loudly, stick with it until restart.
func NewStorage(ctx context.Context, sqliteDir string) (Storage, error) {
	pgHost := os.Getenv("PGHOST")
	if pgHost == "" {
		slog.Info("devshard storage: PGHOST not set, using sqlite", "dir", sqliteDir)
		return NewSQLite(sqliteDir)
	}

	pg, err := NewPostgres(ctx)
	if err != nil {
		slog.Warn("devshard storage: postgres unavailable, falling back to sqlite for this run",
			"host", pgHost, "error", err, "dir", sqliteDir)
		return NewSQLite(sqliteDir)
	}
	slog.Info("devshard storage: using postgres", "host", pgHost)
	return pg, nil
}
