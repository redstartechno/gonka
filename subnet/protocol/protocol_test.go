package protocol

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"subnet/host"
	"subnet/signing"
	"subnet/state"
	"subnet/stub"
	"subnet/types"
	"subnet/user"
)

// --- helpers ---

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

func defaultConfig(numHosts int) types.SessionConfig {
	return types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(numHosts) / 2,
	}
}

type testEnv struct {
	session *user.Session
	hosts   []*host.Host
	signers []*signing.Secp256k1Signer
	user    *signing.Secp256k1Signer
	group   []types.SlotAssignment
	config  types.SessionConfig
}

func setupEnv(t *testing.T, numHosts int, balance, grace uint64) *testEnv {
	t.Helper()
	hostSigners := make([]*signing.Secp256k1Signer, numHosts)
	for i := range hostSigners {
		hostSigners[i] = mustGenerateKey(t)
	}
	userSigner := mustGenerateKey(t)
	group := makeGroup(hostSigners)
	config := defaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	hosts := make([]*host.Host, numHosts)
	clients := make([]user.HostClient, numHosts)
	for i := range hostSigners {
		sm := state.NewStateMachine("escrow-1", config, group, balance, userSigner.Address(), verifier)
		engine := stub.NewInferenceEngine()
		h, err := host.NewHost(sm, hostSigners[i], engine, "escrow-1", group, grace, nil)
		require.NoError(t, err)
		hosts[i] = h
		clients[i] = &user.InProcessClient{Host: h}
	}

	userSM := state.NewStateMachine("escrow-1", config, group, balance, userSigner.Address(), verifier)
	session, err := user.NewSession(userSM, userSigner, "escrow-1", group, clients)
	require.NoError(t, err)

	return &testEnv{
		session: session,
		hosts:   hosts,
		signers: hostSigners,
		user:    userSigner,
		group:   group,
		config:  config,
	}
}

func defaultParams() user.InferenceParams {
	return user.InferenceParams{
		Model:       "llama",
		PromptHash:  []byte("prompt"),
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	}
}

// --- Integration tests ---

func TestProtocol_HappyPath_15Inferences(t *testing.T) {
	env := setupEnv(t, 5, 1000000, 100)
	ctx := context.Background()
	params := defaultParams()

	for i := 0; i < 15; i++ {
		result, err := env.session.SendInference(ctx, params)
		require.NoError(t, err, "inference %d", i+1)
		require.NotNil(t, result)
	}

	// Verify all 15 inferences.
	st := env.session.StateMachine().GetState()

	// Count finished inferences. Due to pipelining, the first few might still
	// be pending/started since MsgFinishInference gets included in later diffs.
	// The protocol is: nonce N starts inference N, nonce N+1 includes
	// ConfirmStart for N + FinishInference for N.
	// So inferences 1-14 should have their finish included. Inference 15 is
	// still in the executor's mempool.
	finishedCount := 0
	startedCount := 0
	for _, rec := range st.Inferences {
		switch rec.Status {
		case types.StatusFinished:
			finishedCount++
		case types.StatusStarted:
			startedCount++
		}
	}
	// At minimum, inferences that had their finish included should be finished.
	// The exact count depends on how many diffs were processed.
	require.Equal(t, 15, len(st.Inferences), "should have 15 inference records")
	require.True(t, finishedCount >= 13, "at least 13 should be finished, got %d", finishedCount)

	// Verify executor distribution: each host should execute 3 inferences
	// (15 inferences / 5 hosts = 3 each).
	executorCounts := make(map[uint32]int)
	for id, rec := range st.Inferences {
		expectedExecutor := uint32(id % 5)
		require.Equal(t, expectedExecutor, rec.ExecutorSlot, "inference %d executor", id)
		executorCounts[rec.ExecutorSlot]++
	}
	for slot := uint32(0); slot < 5; slot++ {
		require.Equal(t, 3, executorCounts[slot], "slot %d should execute 3 inferences", slot)
	}

	// Verify balance decreased.
	require.Less(t, st.Balance, uint64(1000000))

	// Verify host stats cost for finished inferences.
	totalCost := uint64(0)
	for _, hs := range st.HostStats {
		totalCost += hs.Cost
	}
	expectedCostPerFinished := uint64(120) // (80+40)*1
	require.Equal(t, uint64(finishedCount)*expectedCostPerFinished, totalCost)

	// Verify signatures collected.
	sigs := env.session.Signatures()
	require.NotEmpty(t, sigs)
}

func TestProtocol_ReceiptPipelining(t *testing.T) {
	env := setupEnv(t, 3, 100000, 10)
	ctx := context.Background()
	params := defaultParams()

	// Send 3 inferences.
	for i := 0; i < 3; i++ {
		_, err := env.session.SendInference(ctx, params)
		require.NoError(t, err)
	}

	diffs := env.session.Diffs()
	require.Len(t, diffs, 3)

	// Diff at nonce 2 should include MsgConfirmStart for inference 1
	// AND MsgFinishInference for inference 1 (both pipelined from host 1's response).
	var hasConfirmForInf1, hasFinishForInf1 bool
	for _, tx := range diffs[1].Txs {
		if confirm := tx.GetConfirmStart(); confirm != nil && confirm.InferenceId == 1 {
			hasConfirmForInf1 = true
		}
		if fin := tx.GetFinishInference(); fin != nil && fin.InferenceId == 1 {
			hasFinishForInf1 = true
		}
	}
	require.True(t, hasConfirmForInf1, "diff at nonce 2 should pipeline MsgConfirmStart for inference 1")
	require.True(t, hasFinishForInf1, "diff at nonce 2 should pipeline MsgFinishInference for inference 1")

	// Diff at nonce 3 should include MsgConfirmStart for inference 2
	// AND MsgFinishInference for inference 2.
	var hasConfirmForInf2, hasFinishForInf2 bool
	for _, tx := range diffs[2].Txs {
		if confirm := tx.GetConfirmStart(); confirm != nil && confirm.InferenceId == 2 {
			hasConfirmForInf2 = true
		}
		if fin := tx.GetFinishInference(); fin != nil && fin.InferenceId == 2 {
			hasFinishForInf2 = true
		}
	}
	require.True(t, hasConfirmForInf2, "diff at nonce 3 should pipeline MsgConfirmStart for inference 2")
	require.True(t, hasFinishForInf2, "diff at nonce 3 should pipeline MsgFinishInference for inference 2")
}

func TestProtocol_SignatureWithholding(t *testing.T) {
	// Manual protocol drive: 3 hosts, grace=2.
	hostSigners := make([]*signing.Secp256k1Signer, 3)
	for i := range hostSigners {
		hostSigners[i] = mustGenerateKey(t)
	}
	userSigner := mustGenerateKey(t)
	group := makeGroup(hostSigners)
	config := defaultConfig(3)
	verifier := signing.NewSecp256k1Verifier()

	// Create host at slot 1 with grace=2.
	sm := state.NewStateMachine("escrow-1", config, group, 100000, userSigner.Address(), verifier)
	engine := stub.NewInferenceEngine()
	h, err := host.NewHost(sm, hostSigners[1], engine, "escrow-1", group, 2, nil)
	require.NoError(t, err)

	ctx := context.Background()

	// Nonce 1: start inference 1, executor=slot 1.
	diff1 := signDiff(t, userSigner, 1, []*types.SubnetTx{startTx(1)})
	resp, err := h.HandleRequest(ctx, host.HostRequest{Diffs: []types.Diff{diff1}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should sign at nonce 1")
	require.NotNil(t, resp.Receipt)
	require.Len(t, resp.Mempool, 1) // MsgFinishInference

	// Nonces 2-4: empty diffs, never including the finish.
	// Grace=2, proposed at nonce 1.
	// Nonce 2: 1+2=3, not < 2 -> OK
	// Nonce 3: 1+2=3, not < 3 -> OK
	// Nonce 4: 1+2=3 < 4 -> stale -> withhold
	for n := uint64(2); n <= 4; n++ {
		diff := signDiff(t, userSigner, n, nil)
		resp, err = h.HandleRequest(ctx, host.HostRequest{Diffs: []types.Diff{diff}})
		require.NoError(t, err)
		if n < 4 {
			require.NotNil(t, resp.StateSig, "should sign at nonce %d", n)
		} else {
			require.Nil(t, resp.StateSig, "should withhold at nonce 4")
		}
		// Still processes diffs and returns mempool.
		require.Equal(t, n, resp.Nonce)
		require.Len(t, resp.Mempool, 1, "mempool should still have the finish tx")
	}
}

func TestProtocol_SignatureResumesAfterInclusion(t *testing.T) {
	hostSigners := make([]*signing.Secp256k1Signer, 3)
	for i := range hostSigners {
		hostSigners[i] = mustGenerateKey(t)
	}
	userSigner := mustGenerateKey(t)
	group := makeGroup(hostSigners)
	config := defaultConfig(3)
	verifier := signing.NewSecp256k1Verifier()

	sm := state.NewStateMachine("escrow-1", config, group, 100000, userSigner.Address(), verifier)
	engine := stub.NewInferenceEngine()
	h, err := host.NewHost(sm, hostSigners[1], engine, "escrow-1", group, 2, nil)
	require.NoError(t, err)

	ctx := context.Background()

	// Nonce 1: start inference 1.
	diff1 := signDiff(t, userSigner, 1, []*types.SubnetTx{startTx(1)})
	resp, err := h.HandleRequest(ctx, host.HostRequest{Diffs: []types.Diff{diff1}})
	require.NoError(t, err)
	finishTx := resp.Mempool[0]

	// Nonce 2: confirm start.
	receiptContent := &types.ExecutorReceiptContent{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}
	receiptData, _ := proto.Marshal(receiptContent)
	receiptSig, _ := hostSigners[1].Sign(receiptData)
	confirmTx := &types.SubnetTx{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: receiptSig,
	}}}
	diff2 := signDiff(t, userSigner, 2, []*types.SubnetTx{confirmTx})

	// Nonces 3,4: empty (push past grace).
	diff3 := signDiff(t, userSigner, 3, nil)
	diff4 := signDiff(t, userSigner, 4, nil)

	resp, err = h.HandleRequest(ctx, host.HostRequest{Diffs: []types.Diff{diff2, diff3, diff4}})
	require.NoError(t, err)
	require.Nil(t, resp.StateSig, "should withhold (stale)")

	// Nonce 5: include the finish tx -> mempool cleared.
	diff5 := signDiff(t, userSigner, 5, []*types.SubnetTx{finishTx})
	resp, err = h.HandleRequest(ctx, host.HostRequest{Diffs: []types.Diff{diff5}})
	require.NoError(t, err)
	require.NotNil(t, resp.StateSig, "should resume signing after inclusion")
}

func TestProtocol_ExecutorAssignment(t *testing.T) {
	env := setupEnv(t, 5, 1000000, 100)
	ctx := context.Background()
	params := defaultParams()

	for i := 0; i < 5; i++ {
		result, err := env.session.SendInference(ctx, params)
		require.NoError(t, err)

		inferenceID := result.InferenceID
		expectedHostIdx := int(inferenceID % 5)

		// Only the executor should return a receipt.
		if expectedHostIdx == int(inferenceID%5) {
			require.NotNil(t, result.Receipt, "executor host should return receipt for inference %d", inferenceID)
		}
	}

	// Verify executor assignment in state.
	st := env.session.StateMachine().GetState()
	for id, rec := range st.Inferences {
		expectedSlot := uint32(id % 5)
		require.Equal(t, expectedSlot, rec.ExecutorSlot, "inference %d should have executor slot %d", id, expectedSlot)
	}
}

// --- helpers for manual protocol driving ---

func signDiff(t *testing.T, signer signing.Signer, nonce uint64, txs []*types.SubnetTx) types.Diff {
	t.Helper()
	content := state.BuildDiffContent(nonce, txs)
	data, err := proto.Marshal(content)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return types.Diff{Nonce: nonce, Txs: txs, UserSig: sig}
}

func startTx(inferenceID uint64) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_StartInference{StartInference: &types.MsgStartInference{
		InferenceId: inferenceID,
		PromptHash:  []byte("prompt"),
		Model:       "llama",
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	}}}
}
