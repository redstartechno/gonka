package storage

import (
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/types"
)

func TestCreateSession_GetState(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{
		{SlotID: 0, ValidatorAddress: "addr0"},
		{SlotID: 1, ValidatorAddress: "addr1"},
	}

	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	state, err := store.GetState("escrow-1")
	require.NoError(t, err)
	require.Equal(t, "escrow-1", state.EscrowID)
	require.Equal(t, uint64(1000), state.Balance)
	require.Len(t, state.Group, 2)
	require.Equal(t, uint64(0), state.LatestNonce)
}

func TestAppendDiff_GetDiffs(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{{SlotID: 0, ValidatorAddress: "addr0"}}
	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	for i := uint64(1); i <= 5; i++ {
		err = store.AppendDiff("escrow-1", types.DiffRecord{
			Diff: types.Diff{
				Nonce:   i,
				UserSig: []byte("sig"),
			},
			StateHash:  []byte{byte(i)},
			Signatures: map[uint32][]byte{0: {byte(i)}},
		})
		require.NoError(t, err)
	}

	diffs, err := store.GetDiffs("escrow-1", 2, 4)
	require.NoError(t, err)
	require.Len(t, diffs, 3)
	require.Equal(t, uint64(2), diffs[0].Nonce)
	require.Equal(t, uint64(3), diffs[1].Nonce)
	require.Equal(t, uint64(4), diffs[2].Nonce)
}

func TestGetSignatures(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{{SlotID: 0, ValidatorAddress: "addr0"}}
	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	err = store.AppendDiff("escrow-1", types.DiffRecord{
		Diff:       types.Diff{Nonce: 1, UserSig: []byte("sig")},
		StateHash:  []byte{0x01},
		Signatures: map[uint32][]byte{},
	})
	require.NoError(t, err)

	err = store.AddSignature("escrow-1", 1, 0, []byte("sig-0"))
	require.NoError(t, err)
	err = store.AddSignature("escrow-1", 1, 2, []byte("sig-2"))
	require.NoError(t, err)

	sigs, err := store.GetSignatures("escrow-1", 1)
	require.NoError(t, err)
	require.Len(t, sigs, 2)
	require.Equal(t, []byte("sig-0"), sigs[0])
	require.Equal(t, []byte("sig-2"), sigs[2])

	// Mutating returned map should not affect storage.
	sigs[99] = []byte("bad")
	sigs2, err := store.GetSignatures("escrow-1", 1)
	require.NoError(t, err)
	require.Len(t, sigs2, 2)
}

func TestGetSignatures_NotFound(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{{SlotID: 0, ValidatorAddress: "addr0"}}
	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	_, err = store.GetSignatures("escrow-1", 99)
	require.Error(t, err)
}

func TestMarkFinalized_LastFinalized(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{{SlotID: 0, ValidatorAddress: "addr0"}}
	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	// Initially zero.
	last, err := store.LastFinalized("escrow-1")
	require.NoError(t, err)
	require.Equal(t, uint64(0), last)

	// Mark nonce 3 finalized.
	err = store.MarkFinalized("escrow-1", 3)
	require.NoError(t, err)
	last, err = store.LastFinalized("escrow-1")
	require.NoError(t, err)
	require.Equal(t, uint64(3), last)

	// Mark nonce 5 finalized.
	err = store.MarkFinalized("escrow-1", 5)
	require.NoError(t, err)
	last, err = store.LastFinalized("escrow-1")
	require.NoError(t, err)
	require.Equal(t, uint64(5), last)

	// Idempotent: marking 3 again doesn't regress.
	err = store.MarkFinalized("escrow-1", 3)
	require.NoError(t, err)
	last, err = store.LastFinalized("escrow-1")
	require.NoError(t, err)
	require.Equal(t, uint64(5), last)
}

func TestMarkFinalized_SessionNotFound(t *testing.T) {
	store := NewMemory()
	err := store.MarkFinalized("nonexistent", 1)
	require.Error(t, err)

	_, err = store.LastFinalized("nonexistent")
	require.Error(t, err)
}

func TestAddSignature(t *testing.T) {
	store := NewMemory()
	group := []types.SlotAssignment{{SlotID: 0, ValidatorAddress: "addr0"}}
	err := store.CreateSession("escrow-1", types.SessionConfig{}, group, 1000)
	require.NoError(t, err)

	err = store.AppendDiff("escrow-1", types.DiffRecord{
		Diff: types.Diff{
			Nonce:   1,
			UserSig: []byte("sig"),
		},
		StateHash:  []byte{0x01},
		Signatures: map[uint32][]byte{},
	})
	require.NoError(t, err)

	err = store.AddSignature("escrow-1", 1, 3, []byte("host-sig-3"))
	require.NoError(t, err)

	diffs, err := store.GetDiffs("escrow-1", 1, 1)
	require.NoError(t, err)
	require.Len(t, diffs, 1)
	require.Equal(t, []byte("host-sig-3"), diffs[0].Signatures[3])
}
