package payloads

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

const defaultPGRetryInterval = 240 * time.Second

// OpenConfig selects the payload backend for a devshardd child.
// Dir is typically {data-dir}/payloads (created if missing).
type OpenConfig struct {
	Dir string
}

// Open creates the payload Storage for devshardd.
//
// Selection mirrors decentralized-api/payloadstorage (devshard-0.2.13-v2-r2):
//   - PGHOST unset, single-instance: file storage only under Dir.
//   - PGHOST set: Postgres primary with file fallback (lazy PG reconnect).
//   - Multi-versiond overlap without PGHOST: error (ErrSharedPostgresRequired).
//
// Multi-versiond overlap is detected when VERSIOND_FORCE lists more than one
// version, or DEVSHARD_REQUIRE_POSTGRES=1 (set by testenv multi mode).
func Open(ctx context.Context, cfg OpenConfig) (Storage, func(), error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("payloads: mkdir %s: %w", cfg.Dir, err)
	}

	pgHost := os.Getenv("PGHOST")
	if pgHost == "" && requiresSharedPostgres() {
		return nil, nil, ErrSharedPostgresRequired
	}

	fileStore := NewFileStorage(cfg.Dir)

	if pgHost == "" {
		slog.Info("payload storage: using file only", "dir", cfg.Dir)
		return fileStore, func() {}, nil
	}

	retryInterval := pgRetryInterval()
	pgStore, err := newPostgresStorage(ctx)
	if err != nil {
		slog.Warn("payload storage: postgres unavailable at boot, will retry on store",
			"host", pgHost, "error", err)
		hybrid := NewHybridStorage(nil, fileStore, retryInterval)
		return hybrid, hybrid.Close, nil
	}

	slog.Info("payload storage: using postgres with file fallback", "host", pgHost)
	hybrid := NewHybridStorage(pgStore, fileStore, retryInterval)
	return hybrid, hybrid.Close, nil
}

func requiresSharedPostgres() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DEVSHARD_REQUIRE_POSTGRES"))) {
	case "1", "true", "yes":
		return true
	}
	return countVersiondForce() > 1
}

func countVersiondForce() int {
	force := os.Getenv("VERSIOND_FORCE")
	if force == "" {
		return 0
	}
	n := 0
	for _, part := range strings.Split(force, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

func pgRetryInterval() time.Duration {
	retryIntervalStr := os.Getenv("PG_RETRY_INTERVAL")
	retryInterval, err := time.ParseDuration(retryIntervalStr)
	if err != nil || retryInterval <= 0 {
		return defaultPGRetryInterval
	}
	return retryInterval
}
