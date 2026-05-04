package storage

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixedEpoch implements EpochProvider for tests.
type fixedEpoch struct{ n uint64 }

func (f *fixedEpoch) CurrentEpochID() uint64 { return f.n }

func newManagedForTest(t *testing.T, retain uint64, ep EpochProvider) (*ManagedStorage, *Memory) {
	t.Helper()
	mem := NewMemory()
	// Long pruneInterval so the background goroutine does not race with
	// our explicit PruneOnce calls. The constructor still runs one initial
	// pass; we will trigger the rest manually.
	m := NewManagedStorage(mem, retain, time.Hour, ep)
	t.Cleanup(func() { _ = m.Close() })
	return m, mem
}

func sessionsAt(t *testing.T, store Storage) []uint64 {
	t.Helper()
	active, err := store.ListActiveSessions()
	require.NoError(t, err)
	epochs := make([]uint64, 0, len(active))
	for _, a := range active {
		epochs = append(epochs, a.EpochID)
	}
	sort.Slice(epochs, func(i, j int) bool { return epochs[i] < epochs[j] })
	return epochs
}

// TestManaged_RetainsLastN: with retain=3 and observed epochs 1..6, only
// epochs 4, 5, 6 must remain.
func TestManaged_RetainsLastN(t *testing.T) {
	m, _ := newManagedForTest(t, 3, nil)

	for e := uint64(1); e <= 6; e++ {
		require.NoError(t, m.CreateSession(paramsForEpoch("escrow-"+itoa(e), e)))
	}

	m.PruneOnce(context.Background())

	// Active sessions must come from epochs {4, 5, 6} only.
	require.Equal(t, []uint64{4, 5, 6}, sessionsAt(t, m))
}

// TestManaged_NoOpUntilEnoughEpochs: with retain=3 and only epochs 1..2
// observed, nothing should be pruned (we have not yet exceeded retention).
func TestManaged_NoOpUntilEnoughEpochs(t *testing.T) {
	m, _ := newManagedForTest(t, 3, nil)

	require.NoError(t, m.CreateSession(paramsForEpoch("a", 1)))
	require.NoError(t, m.CreateSession(paramsForEpoch("b", 2)))

	m.PruneOnce(context.Background())

	require.Equal(t, []uint64{1, 2}, sessionsAt(t, m))
}

// TestManaged_AdvancesWithEpochProvider: with no CreateSession activity,
// the EpochProvider alone advances the cutoff and stale sessions get pruned.
func TestManaged_AdvancesWithEpochProvider(t *testing.T) {
	ep := &fixedEpoch{n: 1}
	m, _ := newManagedForTest(t, 3, ep)

	require.NoError(t, m.CreateSession(paramsForEpoch("a", 1)))
	require.NoError(t, m.CreateSession(paramsForEpoch("b", 2)))
	require.NoError(t, m.CreateSession(paramsForEpoch("c", 3)))

	// Chain says we are now at epoch 7 -- nothing observed locally above 3.
	ep.n = 7
	m.PruneOnce(context.Background())

	// Retain 3 -> keep epochs 5, 6, 7. Local sessions in 1..3 must be gone.
	require.Empty(t, sessionsAt(t, m))
}

// TestManaged_DoesNotPruneInsideRetention: epochs inside the retention
// window must be untouched even after several prune passes.
func TestManaged_DoesNotPruneInsideRetention(t *testing.T) {
	m, _ := newManagedForTest(t, 3, nil)

	require.NoError(t, m.CreateSession(paramsForEpoch("a", 5)))
	require.NoError(t, m.CreateSession(paramsForEpoch("b", 6)))
	require.NoError(t, m.CreateSession(paramsForEpoch("c", 7)))

	for i := 0; i < 5; i++ {
		m.PruneOnce(context.Background())
	}

	require.Equal(t, []uint64{5, 6, 7}, sessionsAt(t, m))
}

// TestManaged_PrunedUpToMonotonic: prunedUpTo must not regress when the
// observed epoch jumps. The pruner remembers what range it has already swept
// so a higher max_observed does not cause it to redo work, and a late-arriving
// CreateSession for an already-pruned epoch is left alone (no sweep regression).
func TestManaged_PrunedUpToMonotonic(t *testing.T) {
	m, _ := newManagedForTest(t, 3, nil)

	require.NoError(t, m.CreateSession(paramsForEpoch("a", 10)))
	m.PruneOnce(context.Background())
	// max_observed=10, cutoff = 11 - 3 = 8, prunedUpTo advances 0 -> 8.

	// Bumping max_observed to 11 should sweep [8, 9) only -- not redo [0, 8).
	require.NoError(t, m.CreateSession(paramsForEpoch("c", 11)))
	m.PruneOnce(context.Background())
	// cutoff is now 9, prunedUpTo advances 8 -> 9.

	// A late session inserted at epoch 5 is below prunedUpTo. The wrapper
	// won't redo the sweep, so it survives. This documents the contract:
	// upstream callers must not insert into pruned epochs.
	require.NoError(t, m.CreateSession(paramsForEpoch("b", 5)))
	m.PruneOnce(context.Background())
	// cutoff is still 9, prunedUpTo stays 9 -- no work redone.

	// epoch 5 survives (the sweep doesn't regress), 10 and 11 are inside retention.
	require.Equal(t, []uint64{5, 10, 11}, sessionsAt(t, m))
}

// itoa is a tiny strconv-free helper to keep the test file dependency-light.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
