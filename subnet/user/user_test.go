package user

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet"
	"subnet/host"
	"subnet/internal/testutil"
	"subnet/signing"
	"subnet/state"
	"subnet/stub"
	"subnet/types"
)

func setupSession(t *testing.T, numHosts int, balance uint64, grace uint64) (*Session, []*signing.Secp256k1Signer, *signing.Secp256k1Signer) {
	t.Helper()
	return setupSessionWithEngine(t, numHosts, balance, grace, nil)
}

func setupSessionWithEngine(t *testing.T, numHosts int, balance uint64, grace uint64, engines []subnet.InferenceEngine) (*Session, []*signing.Secp256k1Signer, *signing.Secp256k1Signer) {
	t.Helper()
	hosts := make([]*signing.Secp256k1Signer, numHosts)
	for i := range hosts {
		hosts[i] = testutil.MustGenerateKey(t)
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	// Create hosts.
	clients := make([]HostClient, numHosts)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
		var engine subnet.InferenceEngine
		if engines != nil {
			engine = engines[i]
		} else {
			engine = stub.NewInferenceEngine()
		}
		h, err := host.NewHost(sm, hosts[i], engine, "escrow-1", group, grace, nil)
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	// Create user session.
	userSM := state.NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
	session, err := NewSession(userSM, user, "escrow-1", group, clients)
	require.NoError(t, err)

	return session, hosts, user
}

func TestUser_RoundRobinSelection(t *testing.T) {
	session, _, _ := setupSession(t, 3, 100000, 10)
	ctx := context.Background()

	params := InferenceParams{
		Model: "llama", PromptHash: []byte("prompt"),
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	// Nonce 1 -> host 1%3=1, nonce 2 -> host 2%3=2, nonce 3 -> host 3%3=0.
	for i := 0; i < 6; i++ {
		_, hostIdx, err := session.NextDiff(params)
		require.NoError(t, err)
		expectedHost := int((session.nonce + 1) % 3)
		require.Equal(t, expectedHost, hostIdx)

		// Actually send to advance nonce.
		_, err = session.SendInference(ctx, params)
		require.NoError(t, err)
	}

	// Verify round-robin pattern over 6 inferences.
	require.Equal(t, uint64(6), session.Nonce())
}

func TestUser_PipelinesReceipt(t *testing.T) {
	session, _, _ := setupSession(t, 3, 100000, 10)
	ctx := context.Background()

	params := InferenceParams{
		Model: "llama", PromptHash: []byte("prompt"),
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	// First inference.
	result1, err := session.SendInference(ctx, params)
	require.NoError(t, err)
	require.NotNil(t, result1.Receipt, "executor should return receipt")

	// After processing response, pendingTxs should have MsgConfirmStart + MsgFinishInference.
	// NextDiff for inference 2 should include these.
	diff2, _, err := session.NextDiff(params)
	require.NoError(t, err)

	// Find MsgConfirmStart in diff2.
	var hasConfirm bool
	for _, tx := range diff2.Txs {
		if confirm := tx.GetConfirmStart(); confirm != nil {
			require.Equal(t, uint64(1), confirm.InferenceId)
			hasConfirm = true
		}
	}
	require.True(t, hasConfirm, "diff 2 should pipeline MsgConfirmStart for inference 1")
}

func TestUser_CollectsSignatures(t *testing.T) {
	session, _, _ := setupSession(t, 3, 100000, 10)
	ctx := context.Background()

	params := InferenceParams{
		Model: "llama", PromptHash: []byte("prompt"),
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	_, err := session.SendInference(ctx, params)
	require.NoError(t, err)

	sigs := session.Signatures()
	require.NotEmpty(t, sigs, "should have signatures")

	// The contacted host (slot 1 for nonce 1) should have signed.
	nonce1Sigs, ok := sigs[1]
	require.True(t, ok, "should have sigs for nonce 1")
	require.NotNil(t, nonce1Sigs[1], "slot 1 should have signed")
}

// ErrorClient always returns an error.
type ErrorClient struct {
	Err error
}

func (c *ErrorClient) Send(_ context.Context, _ host.HostRequest) (*host.HostResponse, error) {
	return nil, c.Err
}

func TestUser_HostError_StateConsistency(t *testing.T) {
	numHosts := 3
	hosts := make([]*signing.Secp256k1Signer, numHosts)
	for i := range hosts {
		hosts[i] = testutil.MustGenerateKey(t)
	}
	userKey := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	// Create real hosts for slots 0 and 2, error client for slot 1.
	clients := make([]HostClient, numHosts)
	for i := range hosts {
		if i == 1 {
			clients[i] = &ErrorClient{Err: fmt.Errorf("host unavailable")}
			continue
		}
		sm := state.NewStateMachine("escrow-1", config, group, 100000, userKey.Address(), verifier)
		engine := stub.NewInferenceEngine()
		h, err := host.NewHost(sm, hosts[i], engine, "escrow-1", group, 100, nil)
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	userSM := state.NewStateMachine("escrow-1", config, group, 100000, userKey.Address(), verifier)
	session, err := NewSession(userSM, userKey, "escrow-1", group, clients)
	require.NoError(t, err)

	ctx := context.Background()
	params := InferenceParams{
		Model: "llama", PromptHash: []byte("prompt"),
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	// Nonce 1 -> host 1 (error client). Should fail.
	_, err = session.SendInference(ctx, params)
	require.Error(t, err, "send to error host should fail")

	// User's local state should have advanced (diff was applied locally before send).
	require.Equal(t, uint64(1), session.Nonce(), "nonce should have advanced")
	require.Len(t, session.Diffs(), 1, "diff should be recorded")

	// Next inference (nonce 2) -> host 2 (working). Should succeed with catch-up.
	result, err := session.SendInference(ctx, params)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, uint64(2), session.Nonce())
}

func TestUser_Finalize(t *testing.T) {
	session, _, _ := setupSession(t, 3, 100000, 100)
	ctx := context.Background()
	params := InferenceParams{
		Model: "llama", PromptHash: []byte("prompt"),
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	for i := 0; i < 3; i++ {
		_, err := session.SendInference(ctx, params)
		require.NoError(t, err)
	}

	err := session.Finalize(ctx)
	require.NoError(t, err)

	st := session.StateMachine().GetState()
	require.True(t, st.Finalizing)
	for id, rec := range st.Inferences {
		require.Equal(t, types.StatusFinished, rec.Status, "inference %d should be finished", id)
	}
}
