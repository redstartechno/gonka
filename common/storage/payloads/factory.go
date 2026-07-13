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
// Selection:
//   - HA mode (DEVSHARD_HA=1 or DEVSHARD_REQUIRE_POSTGRES=1):
//     PGHOST required; Postgres-only; boot fails if Postgres is unreachable.
//     No file fallback (avoids multi-instance split-brain).
//   - PGHOST unset, non-HA: file storage only under Dir.
//   - PGHOST set, non-HA: Postgres primary with file fallback (lazy reconnect).
func Open(ctx context.Context, cfg OpenConfig) (Storage, func(), error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("payloads: mkdir %s: %w", cfg.Dir, err)
	}

	ha := haModeEnabled()
	pgHost := os.Getenv("PGHOST")
	if pgHost == "" && ha {
		return nil, nil, ErrSharedPostgresRequired
	}

	fileStore := NewFileStorage(cfg.Dir)

	if pgHost == "" {
		slog.Info("payload storage: using file only", "dir", cfg.Dir)
		return fileStore, func() {}, nil
	}

	pgStore, err := newPostgresStorage(ctx)
	if err != nil {
		if ha {
			return nil, nil, fmt.Errorf("payloads: HA mode requires postgres at %s: %w", pgHost, err)
		}
		slog.Warn("payload storage: postgres unavailable at boot, will retry on store",
			"host", pgHost, "error", err)
		hybrid := NewHybridStorage(nil, fileStore, pgRetryInterval())
		return hybrid, hybrid.Close, nil
	}

	if ha {
		n, err := MigrateFilePayloadsToPostgres(ctx, cfg.Dir, pgStore)
		if err != nil {
			pgStore.Close()
			return nil, nil, fmt.Errorf("payloads: HA migrate file payloads: %w", err)
		}
		if n > 0 {
			slog.Info("payload storage: HA migrated file payloads to postgres", "host", pgHost, "files", n)
		}
		slog.Info("payload storage: HA mode postgres-only", "host", pgHost)
		return pgStore, pgStore.Close, nil
	}

	slog.Info("payload storage: using postgres with file fallback", "host", pgHost)
	hybrid := NewHybridStorage(pgStore, fileStore, pgRetryInterval())
	return hybrid, hybrid.Close, nil
}

func haModeEnabled() bool {
	return envTruthy("DEVSHARD_HA") || envTruthy("DEVSHARD_REQUIRE_POSTGRES")
}

func requiresSharedPostgres() bool {
	return haModeEnabled()
}

func envTruthy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func pgRetryInterval() time.Duration {
	retryIntervalStr := os.Getenv("PG_RETRY_INTERVAL")
	retryInterval, err := time.ParseDuration(retryIntervalStr)
	if err != nil || retryInterval <= 0 {
		return defaultPGRetryInterval
	}
	return retryInterval
}
