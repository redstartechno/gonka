package storage

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// setupPostgresContainer spins a fresh PG container per test and points the
// pgx env vars at it. Mirrors the pattern from
// decentralized-api/payloadstorage/postgres_storage_test.go so dapi-side
// regressions and devshard-side regressions are caught the same way.
func setupPostgresContainer(t *testing.T) func() {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping postgres testcontainers tests in -short mode (requires Docker)")
	}

	ctx := context.Background()
	container, err := postgres.Run(ctx,
		"postgres:18.1-bookworm",
		postgres.WithDatabase("testdb"),
		postgres.WithUsername("testuser"),
		postgres.WithPassword("testpass"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)

	host, err := container.Host(ctx)
	require.NoError(t, err)
	port, err := container.MappedPort(ctx, "5432")
	require.NoError(t, err)

	t.Setenv("PGHOST", host)
	t.Setenv("PGPORT", port.Port())
	t.Setenv("PGDATABASE", "testdb")
	t.Setenv("PGUSER", "testuser")
	t.Setenv("PGPASSWORD", "testpass")

	return func() { _ = container.Terminate(ctx) }
}

func newTestPostgres(t *testing.T) *Postgres {
	t.Helper()
	cleanup := setupPostgresContainer(t)
	t.Cleanup(cleanup)

	pg, err := NewPostgres(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	return pg
}

// Conformance suite -- every test that the Memory and SQLite backends pass
// must also pass against real Postgres. Catches schema drift between backends.

func TestPostgres_CreateSession_GetSessionMeta(t *testing.T) {
	runCreateSession_GetSessionMeta(t, newTestPostgres(t))
}
func TestPostgres_CreateSession_Idempotent(t *testing.T) {
	runCreateSession_Idempotent(t, newTestPostgres(t))
}
func TestPostgres_AppendDiff_GetDiffs(t *testing.T) {
	runAppendDiff_GetDiffs(t, newTestPostgres(t))
}
func TestPostgres_GetSignatures(t *testing.T) {
	runGetSignatures(t, newTestPostgres(t))
}
func TestPostgres_MarkFinalized_LastFinalized(t *testing.T) {
	runMarkFinalized_LastFinalized(t, newTestPostgres(t))
}
func TestPostgres_AddSignature(t *testing.T) {
	runAddSignature(t, newTestPostgres(t))
}
func TestPostgres_WarmKeyDelta(t *testing.T) {
	runWarmKeyDelta(t, newTestPostgres(t))
}
func TestPostgres_MarkSettled(t *testing.T) {
	runMarkSettled(t, newTestPostgres(t))
}
func TestPostgres_ListActiveSessions(t *testing.T) {
	runListActiveSessions(t, newTestPostgres(t))
}
func TestPostgres_PruneEpoch_RemovesOnlyTarget(t *testing.T) {
	runPruneEpoch_RemovesOnlyTarget(t, newTestPostgres(t))
}
func TestPostgres_PruneEpoch_Idempotent(t *testing.T) {
	runPruneEpoch_Idempotent(t, newTestPostgres(t))
}
func TestPostgres_PruneEpoch_WriteAfter(t *testing.T) {
	runPruneEpoch_WriteAfter(t, newTestPostgres(t))
}

// TestPostgres_PartitionTablesPhysicallyDropped is the assertion specific to
// the Postgres backend: PruneEpoch must DROP the per-epoch partition tables,
// not just delete rows from them. We query pg_class directly so a regression
// to "DELETE FROM ... WHERE epoch_id = ..." would fail this test.
func TestPostgres_PartitionTablesPhysicallyDropped(t *testing.T) {
	pg := newTestPostgres(t)

	// Create sessions in three epochs so we have three sets of partitions.
	require.NoError(t, pg.CreateSession(paramsForEpoch("a", 100)))
	require.NoError(t, pg.CreateSession(paramsForEpoch("b", 101)))
	require.NoError(t, pg.CreateSession(paramsForEpoch("c", 102)))

	for _, esc := range []string{"a", "b", "c"} {
		require.NoError(t, pg.AppendDiff(esc, makeDiffRecord(1)))
		require.NoError(t, pg.AddSignature(esc, 1, 1, []byte("sig")))
	}

	// All nine partition tables should exist.
	require.Equal(t, []string{
		"devshard_diffs_epoch_100", "devshard_diffs_epoch_101", "devshard_diffs_epoch_102",
		"devshard_sessions_epoch_100", "devshard_sessions_epoch_101", "devshard_sessions_epoch_102",
		"devshard_signatures_epoch_100", "devshard_signatures_epoch_101", "devshard_signatures_epoch_102",
	}, listDevshardPartitions(t, pg.pool))

	// Drop the middle epoch.
	require.NoError(t, pg.PruneEpoch(101))

	// Only epoch 101's three partitions are gone; the others survive.
	require.Equal(t, []string{
		"devshard_diffs_epoch_100", "devshard_diffs_epoch_102",
		"devshard_sessions_epoch_100", "devshard_sessions_epoch_102",
		"devshard_signatures_epoch_100", "devshard_signatures_epoch_102",
	}, listDevshardPartitions(t, pg.pool))

	// And the surviving epochs still have their data accessible.
	for _, esc := range []string{"a", "c"} {
		meta, err := pg.GetSessionMeta(esc)
		require.NoError(t, err, "session %s should survive prune", esc)
		require.Equal(t, uint64(1), meta.LatestNonce)
	}

	// Pruning a non-existent epoch is a no-op.
	require.NoError(t, pg.PruneEpoch(999))
}

// TestPostgres_RecoversIndexAcrossReopen verifies that a fresh Postgres
// handle rebuilds its escrow_id -> epoch_id index by scanning
// devshard_sessions on startup, so subsequent reads route correctly without
// re-creating the session.
func TestPostgres_RecoversIndexAcrossReopen(t *testing.T) {
	cleanup := setupPostgresContainer(t)
	defer cleanup()

	ctx := context.Background()

	pg1, err := NewPostgres(ctx)
	require.NoError(t, err)

	require.NoError(t, pg1.CreateSession(paramsForEpoch("e", 42)))
	require.NoError(t, pg1.AppendDiff("e", makeDiffRecord(1)))
	require.NoError(t, pg1.AppendDiff("e", makeDiffRecord(2)))
	require.NoError(t, pg1.MarkFinalized("e", 2))
	require.NoError(t, pg1.Close())

	// Reopen with a fresh handle. Without index rebuild, GetSessionMeta would
	// return ErrSessionNotFound because lookupEpoch can't route the read.
	pg2, err := NewPostgres(ctx)
	require.NoError(t, err)
	defer pg2.Close()

	meta, err := pg2.GetSessionMeta("e")
	require.NoError(t, err)
	require.Equal(t, uint64(42), meta.EpochID)
	require.Equal(t, uint64(2), meta.LatestNonce)
	require.Equal(t, uint64(2), meta.LastFinalized)

	diffs, err := pg2.GetDiffs("e", 1, 2)
	require.NoError(t, err)
	require.Len(t, diffs, 2)
}

// listDevshardPartitions returns every devshard_*_epoch_<N> partition that
// currently exists, sorted, so the assertion is order-stable.
func listDevshardPartitions(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.relname
		FROM pg_class c
		JOIN pg_inherits i ON i.inhrelid = c.oid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname IN ('devshard_sessions', 'devshard_diffs', 'devshard_signatures')
		ORDER BY c.relname
	`)
	require.NoError(t, err)
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	sort.Strings(names)
	if names == nil {
		return []string{}
	}
	return names
}

