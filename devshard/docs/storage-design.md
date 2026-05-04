# Devshard Storage Design

## Goals

1. Store every devshard session's metadata, diff log, and signatures durably.
2. Support two backends behind one interface: PostgreSQL for shared multi-host deployments, SQLite for single-process dev and fallback.
3. Prune old data on a fixed retention horizon (N=3 epochs) without rewriting or scanning surviving partitions.
4. Stay routable across process restarts without consulting the chain on every request.

## Architecture

```
HostManager (decentralized-api/internal/devshard)
    |
    | Storage interface (devshard/storage/interface.go)
    |
    +---> ManagedStorage (background pruner, N=3)
              |
              +---> Postgres   (parents partitioned by epoch_id)
              |     OR
              +---> SQLite     (per-epoch .db files + _meta.db sidecar)
              |     OR
              +---> Memory     (tests)
```

Backend choice is made once per process by `factory.go` based on `PGHOST`. No mid-flight switching.

## Storage Interface

`devshard/storage/interface.go`

```go
type Storage interface {
    CreateSession(params CreateSessionParams) error
    MarkSettled(escrowID string) error
    ListActiveSessions() ([]ActiveSession, error)
    AppendDiff(escrowID string, rec types.DiffRecord) error
    GetDiffs(escrowID string, fromNonce, toNonce uint64) ([]types.DiffRecord, error)
    AddSignature(escrowID string, nonce uint64, slotID uint32, sig []byte) error
    GetSignatures(escrowID string, nonce uint64) (map[uint32][]byte, error)
    GetSessionMeta(escrowID string) (*SessionMeta, error)
    MarkFinalized(escrowID string, nonce uint64) error
    LastFinalized(escrowID string) (uint64, error)
    PruneEpoch(epochID uint64) error
    Close() error
}

type CreateSessionParams struct {
    EscrowID       string
    EpochID        uint64
    Version        string
    CreatorAddr    string
    Config         types.SessionConfig
    Group          []types.SlotAssignment
    InitialBalance uint64
}

type ActiveSession struct {
    EscrowID string
    EpochID  uint64
}
```

EpochID is the partition key. CreateSession is the only call that introduces a new EpochID. All other session-keyed methods route internally via an `escrow_id -> epoch_id` cache.

## Partition Key

`epoch_id = escrow.epoch_index` from the chain (`DevshardEscrow` proto, `devshard/bridge/interface.go:EscrowInfo.EpochID`).

A session's epoch is fixed at create time. All diffs and signatures for a session live in the same partition for the session's lifetime, even if the session settles after an epoch boundary.

Sessions are expected to settle within 1-2 epochs. With retain=3 the session has at least one full epoch of slack between settlement and prune.

## Backends

### Postgres (`devshard/storage/postgres.go`)

Three parents, each `PARTITION BY RANGE (epoch_id)`:

```sql
CREATE TABLE devshard_sessions   (..., PRIMARY KEY (epoch_id, escrow_id))         PARTITION BY RANGE (epoch_id);
CREATE TABLE devshard_diffs      (..., PRIMARY KEY (epoch_id, escrow_id, nonce))  PARTITION BY RANGE (epoch_id);
CREATE TABLE devshard_signatures (..., PRIMARY KEY (epoch_id, escrow_id, nonce, slot_id)) PARTITION BY RANGE (epoch_id);
```

Per-epoch partitions `devshard_sessions_epoch_<N>`, `devshard_diffs_epoch_<N>`, `devshard_signatures_epoch_<N>` are created lazily on the first write to a new epoch and tracked in an in-memory `knownEpochs` set.

`PruneEpoch(N)` issues `DROP TABLE IF EXISTS` on all three per-epoch partitions. Constant-time, no row scan, no vacuum.

`escrow_id -> epoch_id` index is rebuilt on `NewPostgres` by `SELECT epoch_id, escrow_id FROM devshard_sessions` against the parent. The parent table IS the index; no sidecar.

Mirrors `decentralized-api/payloadstorage/postgres_storage.go` style: same env vars, same pgx pool, same lazy partitioning.

### SQLite (`devshard/storage/sqlite.go`)

Layout under `<baseDir>`:

```
_meta.db                 -- escrow_id -> epoch_id index
epoch_<N>.db             -- per-epoch sessions/diffs/signatures
epoch_<N>.db-wal
epoch_<N>.db-shm
```

`_meta.db`:

```sql
CREATE TABLE escrow_epoch (
    escrow_id TEXT PRIMARY KEY,
    epoch_id  INTEGER NOT NULL
);
CREATE INDEX escrow_epoch_by_epoch ON escrow_epoch(epoch_id);
```

Each `epoch_<N>.db` carries the same three tables as Postgres (sessions, diffs, signatures), single-epoch scope so `epoch_id` is implicit.

WAL mode, separate writer (1 conn) and reader (10 conn) pools per per-epoch DB.

`PruneEpoch(N)`:
1. Close pool for epoch N.
2. `os.Remove` of `epoch_<N>.db`, `.db-wal`, `.db-shm`.
3. `DELETE FROM escrow_epoch WHERE epoch_id = N` in `_meta.db`.

Other epoch files are not touched. No SQLite VACUUM ever runs.

### Memory (`devshard/storage/memory.go`)

Map-of-sessions for tests. Same Storage contract. PruneEpoch removes every session whose `epochID` matches.

## Factory (`devshard/storage/factory.go`)

```
NewStorage(ctx, sqliteDir):
  if PGHOST != "":
      pg = NewPostgres(ctx)
      if pg connects:
          return pg
      log warning, fall through
  return NewSQLite(sqliteDir)
```

Decision is locked for the lifetime of the process. No mid-flight reconnect, no hybrid sync. Operators that need PG must restart the process when PG comes back.

Env vars used (same as `payloadstorage`): `PGHOST`, `PGPORT`, `PGDATABASE`, `PGUSER`, `PGPASSWORD`.

## Pruning (`devshard/storage/managed.go`)

`ManagedStorage` wraps any `Storage` with a background goroutine that ticks every 30s:

1. Updates `max_observed_epoch` from CreateSession calls and from an optional `EpochProvider` (chain).
2. Computes `cutoff = max_observed_epoch + 1 - retain` (retain=3 in production).
3. Calls `inner.PruneEpoch(e)` for every `e` in `[prunedUpTo, cutoff)`.
4. Advances `prunedUpTo = cutoff`. Cursor is monotonic: a late-arriving epoch below `prunedUpTo` is not re-swept.

`EpochProvider` lets the pruner advance even on quiet hosts:

- dapi (in `decentralized-api/main.go`): wraps `chainphase.ChainPhaseTracker.GetCurrentEpochState().LatestEpoch.EpochIndex`.
- standalone devshardd (in `decentralized-api/cmd/devshardd/main.go`): polls `QueryEpochInfo` every 60s, exposes `LatestEpoch.Index`.

A session created in an already-pruned epoch (operator error) survives until the next process restart. The wrapper does not regress its sweep cursor.

## Reboot / Recovery

`HostManager.RecoverSessions` (`decentralized-api/internal/devshard/manager.go:179`):

1. `store.ListActiveSessions()` returns `[]ActiveSession{EscrowID, EpochID}`.
2. For each, `recoverSession(escrowID)` reads meta, replays diffs through a fresh `state.NewStateMachine`.

The chain bridge is consulted only when a brand-new escrow is opened by `HostManager.create`, never on recovery.

### SQLite reboot path

1. `NewSQLite(baseDir)` opens `_meta.db` only.
2. `loadIndexFromMeta` populates the in-memory `escrow_id -> epoch_id` map.
3. `reconcileMetaFromEpochFiles` is a defensive pass: for every `epoch_<N>.db` on disk, scan its `sessions` table and repair any missing `_meta` rows. Handles partial writes and the upgrade path from a pre-`_meta.db` layout.
4. Per-epoch DBs open lazily on the first request that touches them.

### Postgres reboot path

1. `NewPostgres(ctx)` runs `pgCreateParents` (idempotent) then `SELECT epoch_id, escrow_id FROM devshard_sessions`.
2. Index is rebuilt in one query against the parent. Partitions are routed automatically.

## Request Routing

```
POST /sessions/<escrow_id>/diff
  HostManager.SessionServer(escrowID)
    in-memory map hit: return cached server
    miss: HostManager.create(escrowID)
      bridge.GetEscrow(escrowID)            -- one chain RPC per session lifetime
      store.CreateSession(EscrowID, EpochID, ...)
        SQLite:   INSERT _meta.db; INSERT epoch_<EpochID>.db sessions
        Postgres: INSERT devshard_sessions (routes to partition)
  storage.AppendDiff(escrowID, rec)
    lookup escrow_id -> epoch_id from in-memory cache (O(1))
    SQLite:   open epoch_<EpochID>.db lazily; INSERT into diffs + signatures
    Postgres: INSERT into devshard_diffs (routes by epoch_id)
```

Chain bridge: one call per session lifetime, at first contact only.

## Legacy Migration

`MigrateLegacySQLite(legacyPath, dest, epochResolver)` in `devshard/storage/migrate.go`:

1. If `legacyPath` does not exist, return.
2. Open the legacy single-file SQLite read-only.
3. For each session row, call `epochResolver(escrow_id)` (typically `bridge.GetEscrow().EpochID`).
4. Replay session + diffs + signatures into `dest` via the public Storage API.
5. Rename legacy file to `<legacyPath>.migrated.<unix-ts>`.

Idempotent. Sessions whose escrow no longer resolves on chain are skipped with a warning.

Wired in `decentralized-api/main.go` and `decentralized-api/cmd/devshardd/main.go` before `HostManager.RecoverSessions`.

## Test Coverage

| Layer | File | Notes |
|---|---|---|
| Conformance suite | `devshard/storage/shared_test.go` | One set of tests run against Memory, SQLite, and Postgres. Includes prune semantics. |
| SQLite per-epoch layout | `devshard/storage/sqlite_test.go::TestSQLite_PerEpochFile_Layout` | Verifies `epoch_<N>.db` files exist; prune removes only target's files. |
| SQLite meta sidecar | `sqlite_test.go::TestSQLite_MetaIndex_*` | Persistence across reboot; rebuild from per-epoch files when `_meta.db` is missing. |
| Postgres partition drop | `devshard/storage/postgres_test.go::TestPostgres_PartitionTablesPhysicallyDropped` | Queries `pg_class`/`pg_inherits` to confirm partitions are physically gone after PruneEpoch. |
| Migration round-trip | `devshard/storage/migrate_test.go` | Legacy DB -> new layout, including settled status and unknown-escrow skip path. |
| Managed retention | `devshard/storage/managed_test.go` | Retain-last-N math, EpochProvider advances on quiet hosts, monotonic cursor. |
| Integration (dapi+PG) | `testermint/src/test/kotlin/DevshardPostgresStorageTests.kt` | End-to-end: create escrow, drive inferences, settle, assert rows in `devshard_sessions/diffs/signatures` and absence of SQLite files in dapi container. Pruning test waits for ManagedStorage to drop the older partition. |

## Key Files

| Concern | Path |
|---|---|
| Interface + types | `devshard/storage/interface.go` |
| SQLite backend | `devshard/storage/sqlite.go` |
| Postgres backend | `devshard/storage/postgres.go` |
| Memory backend | `devshard/storage/memory.go` |
| Backend factory | `devshard/storage/factory.go` |
| Background pruner | `devshard/storage/managed.go` |
| Legacy migrator | `devshard/storage/migrate.go` |
| Bridge -> EscrowInfo.EpochID | `devshard/bridge/interface.go`, `devshard/bridge/rest.go`, `decentralized-api/internal/devshard/bridge.go` |
| HostManager wiring | `decentralized-api/internal/devshard/manager.go` |
| dapi main wiring | `decentralized-api/main.go` |
| devshardd main wiring | `decentralized-api/cmd/devshardd/main.go` |
| Local test cluster overlay | `local-test-net/docker-compose.postgres.yml` |
| Testermint PG client | `testermint/src/main/kotlin/PostgresClient.kt` |
