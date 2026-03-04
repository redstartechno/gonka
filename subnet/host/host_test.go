package host

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"subnet/signing"
	"subnet/state"
	"subnet/stub"
	"subnet/types"
)

// --- Test helpers (mirrors state/machine_test.go patterns) ---

func mustGenerateKey(t *testing.T) *signing.Secp256k1Signer {
	t.Helper()
	s, err := signing.GenerateKey()
	require.NoError(t, err)
	return s
}

func makeGroup(signers []*signing.Secp256k1Signer) []types.SlotAssignment {
	group := make([]types.SlotAssignment, len(signers))
	for i, s := range signers {
		group[i] = types.SlotAssignment{
			SlotID:           uint32(i),
			ValidatorAddress: s.Address(),
			PublicKey:        s.PublicKeyBytes(),
			Weight:           1,
		}
	}
	return group
}

func defaultConfig() types.SessionConfig {
	return types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    0,
	}
}

func signDiff(t *testing.T, signer signing.Signer, nonce uint64, txs []*types.SubnetTx) types.Diff {
	t.Helper()
	content := state.BuildDiffContent(nonce, txs)
	data, err := proto.Marshal(content)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return types.Diff{Nonce: nonce, Txs: txs, UserSig: sig}
}

func newTestHost(t *testing.T, hostIdx int, hosts []*signing.Secp256k1Signer, user *signing.Secp256k1Signer, balance uint64, grace uint64) *Host {
	t.Helper()
	return newTestHostWithChecker(t, hostIdx, hosts, user, balance, grace, nil)
}

func newTestHostWithChecker(t *testing.T, hostIdx int, hosts []*signing.Secp256k1Signer, user *signing.Secp256k1Signer, balance uint64, grace uint64, checker AcceptanceChecker) *Host {
	t.Helper()
	group := makeGroup(hosts)
	config := defaultConfig()
	config.VoteThreshold = uint32(len(hosts)) / 2
	verifier := signing.NewSecp256k1Verifier()
	sm := state.NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
	engine := stub.NewInferenceEngine()
	h, err := NewHost(sm, hosts[hostIdx], engine, "escrow-1", group, grace, checker)
	require.NoError(t, err)
	return h
}

func startInferenceTx(inferenceID uint64) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_StartInference{StartInference: &types.MsgStartInference{
		InferenceId: inferenceID,
		PromptHash:  []byte("prompt"),
		Model:       "llama",
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	}}}
}

// --- Tests ---

func TestHost_AppliesDiffs(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 0, hosts, user, 10000, 10)

	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.Equal(t, uint64(1), resp.Nonce)
}

func TestHost_SignsState(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 0, hosts, user, 10000, 10)

	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig)

	// Verify the signature recovers to host[0]'s address.
	verifier := signing.NewSecp256k1Verifier()
	group := makeGroup(hosts)
	config := defaultConfig()
	config.VoteThreshold = uint32(len(hosts)) / 2
	sm2 := state.NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)
	_, err = sm2.ApplyDiff(diff)
	require.NoError(t, err)
	root, err := sm2.ComputeStateRoot()
	require.NoError(t, err)

	addr, err := verifier.RecoverAddress(root, resp.StateSig)
	require.NoError(t, err)
	require.Equal(t, hosts[0].Address(), addr)
}

func TestHost_ExecutorReceipt(t *testing.T) {
	// 3 hosts. Inference 1: executor = group[1%3] = slot 1.
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 1, hosts, user, 10000, 10) // host at slot 1

	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.NotNil(t, resp.Receipt, "executor should return receipt")

	// Verify receipt is a valid executor signature.
	verifier := signing.NewSecp256k1Verifier()
	receiptContent := &types.ExecutorReceiptContent{
		InferenceId: 1,
		PromptHash:  []byte("prompt"),
		Model:       "llama",
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	}
	data, err := proto.Marshal(receiptContent)
	require.NoError(t, err)
	addr, err := verifier.RecoverAddress(data, resp.Receipt)
	require.NoError(t, err)
	require.Equal(t, hosts[1].Address(), addr)
}

func TestHost_NonExecutorNoReceipt(t *testing.T) {
	// 3 hosts. Inference 1: executor = slot 1. Host 0 is NOT executor.
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 0, hosts, user, 10000, 10) // host at slot 0

	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.Nil(t, resp.Receipt, "non-executor should not return receipt")
}

func TestHost_ProducesMsgFinish(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 1, hosts, user, 10000, 10) // executor for inference 1

	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.Len(t, resp.Mempool, 1)

	fin := resp.Mempool[0].GetFinishInference()
	require.NotNil(t, fin)
	require.Equal(t, uint64(1), fin.InferenceId)
	require.Equal(t, uint32(1), fin.ExecutorSlot)
	require.Equal(t, uint64(80), fin.InputTokens)
	require.Equal(t, uint64(40), fin.OutputTokens)
	require.NotNil(t, fin.ProposerSig)
}

func TestHost_WithholdsOnStaleTx(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 1, hosts, user, 100000, 2) // grace=2

	// Nonce 1: start inference 1, executor=slot 1 -> produces mempool entry at nonce 1.
	diff := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should sign at nonce 1 (not stale yet)")

	// Nonces 2,3: empty diffs, mempool entry proposed at 1, grace=2.
	// At nonce 3: 1+2=3, not < 3 -> still OK.
	diff2 := signDiff(t, user, 2, nil)
	diff3 := signDiff(t, user, 3, nil)
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff2, diff3}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should sign at nonce 3 (1+2=3, not < 3)")

	// Nonce 4: 1+2=3 < 4 -> stale -> withhold.
	diff4 := signDiff(t, user, 4, nil)
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff4}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "should withhold at nonce 4 (stale)")
	require.Equal(t, uint64(4), resp.Nonce)
	require.Len(t, resp.Mempool, 1, "mempool should still have the entry")
}

func TestHost_SignsAfterIncluded(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	h := newTestHost(t, 1, hosts, user, 100000, 2) // grace=2

	// Nonce 1: start inference 1 -> executor, mempool entry.
	diff1 := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff1}})
	require.NoError(t, err)
	require.Len(t, resp.Mempool, 1)

	// Get the finish tx from mempool to include in a later diff.
	finishTx := resp.Mempool[0]

	// Nonce 2: confirm start (needed for state machine to accept finish).
	receiptContent := &types.ExecutorReceiptContent{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}
	receiptData, _ := proto.Marshal(receiptContent)
	receiptSig, _ := hosts[1].Sign(receiptData)
	confirmTx := &types.SubnetTx{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: receiptSig,
	}}}
	diff2 := signDiff(t, user, 2, []*types.SubnetTx{confirmTx})

	// Nonce 3: empty (to push past grace).
	diff3 := signDiff(t, user, 3, nil)
	// Nonce 4: empty (stale at this point).
	diff4 := signDiff(t, user, 4, nil)

	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff2, diff3, diff4}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "should withhold (stale)")

	// Nonce 5: include the finish tx -> mempool cleared -> should sign.
	diff5 := signDiff(t, user, 5, []*types.SubnetTx{finishTx})
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff5}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should sign after inclusion")
	require.Equal(t, 0, h.mempool.Len())
}

func TestHost_NotInGroup(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t)}
	outsider := mustGenerateKey(t)
	group := makeGroup(hosts)
	config := defaultConfig()
	verifier := signing.NewSecp256k1Verifier()
	sm := state.NewStateMachine("escrow-1", config, group, 10000, outsider.Address(), verifier)
	engine := stub.NewInferenceEngine()

	_, err := NewHost(sm, outsider, engine, "escrow-1", group, 10, nil)
	require.ErrorIs(t, err, types.ErrHostNotInGroup)
}

// makeMultiSlotGroup builds a group where signers[dupIdx] occupies two slots.
// The extra slot is appended at the end.
func makeMultiSlotGroup(signers []*signing.Secp256k1Signer, dupIdx int) []types.SlotAssignment {
	group := makeGroup(signers)
	// Add a second slot for signers[dupIdx].
	extra := types.SlotAssignment{
		SlotID:           uint32(len(signers)),
		ValidatorAddress: signers[dupIdx].Address(),
		PublicKey:        signers[dupIdx].PublicKeyBytes(),
		Weight:           1,
	}
	return append(group, extra)
}

func newMultiSlotHost(t *testing.T, hostIdx int, hosts []*signing.Secp256k1Signer, user *signing.Secp256k1Signer, group []types.SlotAssignment, balance uint64, grace uint64) *Host {
	t.Helper()
	config := defaultConfig()
	config.VoteThreshold = uint32(len(group)) / 2
	verifier := signing.NewSecp256k1Verifier()
	sm := state.NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
	engine := stub.NewInferenceEngine()
	h, err := NewHost(sm, hosts[hostIdx], engine, "escrow-1", group, grace, nil)
	require.NoError(t, err)
	return h
}

func TestHost_MultiSlotExecutor(t *testing.T) {
	// 3 signers, signer[0] holds slots 0 and 3 (4 slots total).
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)
	group := makeMultiSlotGroup(hosts, 0)
	// group has 4 slots: 0(hosts[0]), 1(hosts[1]), 2(hosts[2]), 3(hosts[0]).

	h := newMultiSlotHost(t, 0, hosts, user, group, 100000, 10)

	// Verify host holds both slots.
	require.True(t, h.slotIDs[0])
	require.True(t, h.slotIDs[3])
	require.Len(t, h.slotIDs, 2)

	// inference_id must equal nonce. Pick nonces that map to the right executor slots.
	// nonce 4: executor = group[4%4]=group[0] -> slot 0 -> hosts[0] executes.
	diff1 := signDiff(t, user, 1, nil) // empty diff to advance nonce
	diff2 := signDiff(t, user, 2, nil)
	diff3 := signDiff(t, user, 3, nil)
	diff4 := signDiff(t, user, 4, []*types.SubnetTx{startInferenceTx(4)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{
		Diffs: []types.Diff{diff1, diff2, diff3, diff4},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Receipt, "host should execute for slot 0 (nonce 4)")
	require.Len(t, resp.Mempool, 1)
	fin4 := resp.Mempool[0].GetFinishInference()
	require.Equal(t, uint32(0), fin4.ExecutorSlot)

	// nonce 6: executor = group[6%4]=group[2] -> slot 2 -> hosts[2], NOT hosts[0].
	diff5 := signDiff(t, user, 5, nil)
	diff6 := signDiff(t, user, 6, []*types.SubnetTx{startInferenceTx(6)})
	resp, err = h.HandleRequest(context.Background(), HostRequest{
		Diffs: []types.Diff{diff5, diff6},
	})
	require.NoError(t, err)
	require.Nil(t, resp.Receipt, "host should NOT execute for slot 2")

	// nonce 7: executor = group[7%4]=group[3] -> slot 3 -> hosts[0] again.
	diff7 := signDiff(t, user, 7, []*types.SubnetTx{startInferenceTx(7)})
	resp, err = h.HandleRequest(context.Background(), HostRequest{
		Diffs: []types.Diff{diff7},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Receipt, "host should execute for slot 3 (nonce 7)")
	var fin7 *types.MsgFinishInference
	for _, tx := range resp.Mempool {
		if f := tx.GetFinishInference(); f != nil && f.InferenceId == 7 {
			fin7 = f
			break
		}
	}
	require.NotNil(t, fin7)
	require.Equal(t, uint32(3), fin7.ExecutorSlot)
}

// mockAcceptanceChecker blocks when blockFn returns true.
type mockAcceptanceChecker struct {
	blockFn func(types.EscrowState) bool
}

func (m *mockAcceptanceChecker) Check(st types.EscrowState) error {
	if m.blockFn(st) {
		return fmt.Errorf("acceptance check failed")
	}
	return nil
}

func TestHost_WithholdsOnAcceptanceBlock(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)

	// Block whenever there's any inference in the state.
	checker := &mockAcceptanceChecker{
		blockFn: func(st types.EscrowState) bool {
			return len(st.Inferences) > 0
		},
	}
	h := newTestHostWithChecker(t, 0, hosts, user, 10000, 100, checker)

	// Empty diff: no inferences -> should sign.
	diff1 := signDiff(t, user, 1, nil)
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff1}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should sign with no inferences")

	// Diff with start inference: checker blocks.
	diff2 := signDiff(t, user, 2, []*types.SubnetTx{startInferenceTx(2)})
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff2}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "should withhold due to acceptance check")
}

func TestHost_AcceptanceBlockPersistsAcrossRounds(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{mustGenerateKey(t), mustGenerateKey(t), mustGenerateKey(t)}
	user := mustGenerateKey(t)

	callCount := 0
	// Block for first 2 calls, then allow.
	checker := &mockAcceptanceChecker{
		blockFn: func(_ types.EscrowState) bool {
			callCount++
			return callCount <= 2
		},
	}
	h := newTestHostWithChecker(t, 0, hosts, user, 100000, 100, checker)

	// Round 1: blocked.
	diff1 := signDiff(t, user, 1, []*types.SubnetTx{startInferenceTx(1)})
	resp, err := h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff1}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "round 1: blocked")

	// Round 2: still blocked.
	diff2 := signDiff(t, user, 2, nil)
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff2}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "round 2: still blocked")

	// Round 3: checker allows.
	diff3 := signDiff(t, user, 3, nil)
	resp, err = h.HandleRequest(context.Background(), HostRequest{Diffs: []types.Diff{diff3}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "round 3: checker allows signing")
}
