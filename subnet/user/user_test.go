package user

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/host"
	"subnet/signing"
	"subnet/state"
	"subnet/stub"
	"subnet/types"
)

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

func setupSession(t *testing.T, numHosts int, balance uint64, grace uint64) (*Session, []*signing.Secp256k1Signer, *signing.Secp256k1Signer) {
	t.Helper()
	hosts := make([]*signing.Secp256k1Signer, numHosts)
	for i := range hosts {
		hosts[i] = mustGenerateKey(t)
	}
	user := mustGenerateKey(t)
	group := makeGroup(hosts)
	config := defaultConfig()
	config.VoteThreshold = uint32(numHosts) / 2
	verifier := signing.NewSecp256k1Verifier()

	// Create hosts.
	clients := make([]HostClient, numHosts)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
		engine := stub.NewInferenceEngine()
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
