package state

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/types"
)

func TestBuildSettlement_MerkleProof(t *testing.T) {
	hostStats := map[uint32]*types.HostStats{
		0: {Cost: 100, RequiredValidations: 2, CompletedValidations: 1},
		1: {Cost: 200, Missed: 1},
	}
	inferences := map[uint64]*types.InferenceRecord{
		1: {Status: types.StatusFinished, ExecutorSlot: 0, ActualCost: 100, VotedSlots: map[uint32]bool{}},
		2: {Status: types.StatusFinished, ExecutorSlot: 1, ActualCost: 200, VotedSlots: map[uint32]bool{}},
	}

	st := types.EscrowState{
		Balance:    9700,
		HostStats:  hostStats,
		Inferences: inferences,
	}

	payload, err := BuildSettlement(st, map[uint32][]byte{0: {1}, 1: {2}}, 10)
	require.NoError(t, err)

	// Verify Merkle structure: stateRoot = sha256(hostStatsHash || restHash).
	hostStatsHash, err := ComputeHostStatsHash(hostStats)
	require.NoError(t, err)

	h := sha256.New()
	h.Write(hostStatsHash)
	h.Write(payload.RestHash)
	expectedRoot := h.Sum(nil)

	require.Equal(t, expectedRoot, payload.StateRoot)
}
