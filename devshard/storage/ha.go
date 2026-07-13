package storage

import (
	"errors"
	"os"
	"strings"
)

// ErrHAPostgresRequired is returned when HA mode is enabled but PGHOST is unset.
var ErrHAPostgresRequired = errors.New(
	"devshard HA mode requires shared Postgres (set PGHOST and PG* env, or unset DEVSHARD_HA / DEVSHARD_REQUIRE_POSTGRES)",
)

// HAModeEnabled reports whether this process must run Postgres-only for shared
// multi-instance / HA deployments.
//
// Enabled when either:
//   - DEVSHARD_HA=1|true|yes
//   - DEVSHARD_REQUIRE_POSTGRES=1|true|yes
//
// VERSIOND_FORCE does not imply HA; multi-versiond compose must set one of the
// flags above explicitly (gencompose / docker-compose.versiond.yml do).
//
// In HA mode NewStorage and payload Open fail closed if Postgres is unavailable,
// fully migrate local SQLite sessions and file payloads into Postgres at boot
// (before serving), and never fall back to SQLite/file for new writes.
func HAModeEnabled() bool {
	return envTruthy("DEVSHARD_HA") || envTruthy("DEVSHARD_REQUIRE_POSTGRES")
}

func envTruthy(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
