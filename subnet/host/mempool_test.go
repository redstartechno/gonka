package host

import (
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/types"
)

func finishTx(inferenceID uint64) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_FinishInference{
		FinishInference: &types.MsgFinishInference{InferenceId: inferenceID},
	}}
}

func TestMempool_AddAndTxs(t *testing.T) {
	m := NewMempool()
	require.Equal(t, 0, m.Len())
	require.Nil(t, m.Txs())

	tx1 := finishTx(1)
	tx2 := finishTx(2)
	m.Add(MempoolEntry{Tx: tx1, ProposedAt: 5})
	m.Add(MempoolEntry{Tx: tx2, ProposedAt: 6})

	require.Equal(t, 2, m.Len())
	txs := m.Txs()
	require.Len(t, txs, 2)
	require.Equal(t, tx1, txs[0])
	require.Equal(t, tx2, txs[1])
}

func validationTx(inferenceID uint64, slot uint32) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_Validation{
		Validation: &types.MsgValidation{InferenceId: inferenceID, ValidatorSlot: slot},
	}}
}

func TestMempool_RemoveIncluded(t *testing.T) {
	m := NewMempool()
	m.Add(MempoolEntry{Tx: validationTx(1, 0), ProposedAt: 5})
	m.Add(MempoolEntry{Tx: validationTx(2, 1), ProposedAt: 6})
	m.Add(MempoolEntry{Tx: finishTx(3), ProposedAt: 7})

	// Remove validation for inference 2 only.
	m.RemoveIncluded([]*types.SubnetTx{validationTx(2, 1)})

	require.Equal(t, 2, m.Len())
	txs := m.Txs()
	require.Equal(t, uint64(1), txs[0].GetValidation().InferenceId)
	require.Equal(t, uint64(3), txs[1].GetFinishInference().InferenceId)
}

func TestMempool_HasStale(t *testing.T) {
	m := NewMempool()
	m.Add(MempoolEntry{Tx: finishTx(1), ProposedAt: 5})

	// grace=3, currentNonce=8: 5+3=8, not < 8 -> not stale
	require.False(t, m.HasStale(8, 3))

	// grace=3, currentNonce=9: 5+3=8 < 9 -> stale
	require.True(t, m.HasStale(9, 3))
}

func TestMempool_RemoveOnlyMatching(t *testing.T) {
	m := NewMempool()
	m.Add(MempoolEntry{Tx: finishTx(1), ProposedAt: 5})
	m.Add(MempoolEntry{Tx: validationTx(1, 0), ProposedAt: 6})

	// Remove with a tx that doesn't match any entry.
	m.RemoveIncluded([]*types.SubnetTx{finishTx(99)})
	require.Equal(t, 2, m.Len())

	// Same inference_id but different tx type -- must not remove the validation.
	m.RemoveIncluded([]*types.SubnetTx{finishTx(1)})
	require.Equal(t, 1, m.Len())
	require.NotNil(t, m.Txs()[0].GetValidation())
}
