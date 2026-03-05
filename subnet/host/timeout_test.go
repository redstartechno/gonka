package host

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/types"
)

// mockExecutorClient is a test double for ExecutorClient.
type mockExecutorClient struct {
	mempool    []*types.SubnetTx
	mempoolErr error
	sendResp   *HostResponse
	sendErr    error
}

func (m *mockExecutorClient) GetMempool(_ context.Context) ([]*types.SubnetTx, error) {
	return m.mempool, m.mempoolErr
}

func (m *mockExecutorClient) Send(_ context.Context, _ HostRequest) (*HostResponse, error) {
	return m.sendResp, m.sendErr
}

func stateWithPending(inferenceID uint64, executorSlot uint32) types.EscrowState {
	return types.EscrowState{
		EscrowID: "escrow-1",
		Config:   types.SessionConfig{TokenPrice: 1, VoteThreshold: 1},
		Inferences: map[uint64]*types.InferenceRecord{
			inferenceID: {
				Status:       types.StatusPending,
				ExecutorSlot: executorSlot,
				ReservedCost: 150,
			},
		},
		HostStats: map[uint32]*types.HostStats{0: {}, 1: {}},
		Balance:   10000,
	}
}

func stateWithStarted(inferenceID uint64, executorSlot uint32) types.EscrowState {
	return types.EscrowState{
		EscrowID: "escrow-1",
		Config:   types.SessionConfig{TokenPrice: 1, VoteThreshold: 1},
		Inferences: map[uint64]*types.InferenceRecord{
			inferenceID: {
				Status:       types.StatusStarted,
				ExecutorSlot: executorSlot,
				ReservedCost: 150,
			},
		},
		HostStats: map[uint32]*types.HostStats{0: {}, 1: {}},
		Balance:   10000,
	}
}

// --- Refused timeout tests ---

func TestVerifyRefused_ReceiptInLocalMempool(t *testing.T) {
	st := stateWithPending(1, 1)
	mempool := []*types.SubnetTx{
		{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{InferenceId: 1}}},
	}

	accept, err := VerifyRefusedTimeout(context.Background(), st, 1, nil, mempool, nil, st.Config)
	require.NoError(t, err)
	require.False(t, accept, "should reject: receipt in local mempool")
}

func TestVerifyRefused_ExecutorUnreachable_ValidRequest(t *testing.T) {
	st := stateWithPending(1, 1)
	executor := &mockExecutorClient{mempoolErr: errors.New("unreachable")}

	accept, err := VerifyRefusedTimeout(context.Background(), st, 1, []byte("prompt"), nil, executor, st.Config)
	require.NoError(t, err)
	require.True(t, accept, "should accept: executor unreachable")
}

func TestVerifyRefused_ExecutorRespondsWithReceipt(t *testing.T) {
	st := stateWithPending(1, 1)
	executor := &mockExecutorClient{
		mempool: []*types.SubnetTx{
			{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{InferenceId: 1}}},
		},
	}

	accept, err := VerifyRefusedTimeout(context.Background(), st, 1, []byte("prompt"), nil, executor, st.Config)
	require.NoError(t, err)
	require.False(t, accept, "should reject: executor has receipt")
}

func TestVerifyRefused_InferenceNotPending(t *testing.T) {
	st := stateWithStarted(1, 1) // started, not pending

	_, err := VerifyRefusedTimeout(context.Background(), st, 1, nil, nil, nil, st.Config)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected pending")
}

// --- Execution timeout tests ---

func TestVerifyExecution_FinishInLocalMempool(t *testing.T) {
	st := stateWithStarted(1, 1)
	mempool := []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinishInference{FinishInference: &types.MsgFinishInference{InferenceId: 1}}},
	}

	accept, err := VerifyExecutionTimeout(context.Background(), st, 1, mempool, nil, st.Config)
	require.NoError(t, err)
	require.False(t, accept, "should reject: finish in local mempool")
}

func TestVerifyExecution_ExecutorHasFinish(t *testing.T) {
	st := stateWithStarted(1, 1)
	executor := &mockExecutorClient{
		mempool: []*types.SubnetTx{
			{Tx: &types.SubnetTx_FinishInference{FinishInference: &types.MsgFinishInference{InferenceId: 1}}},
		},
	}

	accept, err := VerifyExecutionTimeout(context.Background(), st, 1, nil, executor, st.Config)
	require.NoError(t, err)
	require.False(t, accept, "should reject: executor has finish")
}

func TestVerifyExecution_ExecutorUnreachable_DeadlinePassed(t *testing.T) {
	st := stateWithStarted(1, 1)
	executor := &mockExecutorClient{mempoolErr: errors.New("unreachable")}

	accept, err := VerifyExecutionTimeout(context.Background(), st, 1, nil, executor, st.Config)
	require.NoError(t, err)
	require.True(t, accept, "should accept: executor unreachable")
}

func TestVerifyExecution_InferenceNotStarted(t *testing.T) {
	st := stateWithPending(1, 1) // pending, not started

	_, err := VerifyExecutionTimeout(context.Background(), st, 1, nil, nil, st.Config)
	require.Error(t, err)
	require.Contains(t, err.Error(), "expected started")
}

func TestVerifyExecution_NilExecutorClient(t *testing.T) {
	st := stateWithStarted(1, 1)

	accept, err := VerifyExecutionTimeout(context.Background(), st, 1, nil, nil, st.Config)
	require.NoError(t, err)
	require.True(t, accept, "should accept: no executor client (unreachable)")
}
