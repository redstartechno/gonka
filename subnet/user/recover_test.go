package user

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet"
	"subnet/host"
	"subnet/internal/testutil"
	"subnet/signing"
	"subnet/state"
	"subnet/storage"
	"subnet/stub"
	"subnet/types"
)

func newTestStore(t *testing.T) *storage.SQLite {
	t.Helper()
	db, err := storage.NewSQLite(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// setupRecoverableSession creates a session with SQLite storage and sends
// numInferences inferences. Returns the store, group, hosts, user signer,
// and the final nonce reached.
func setupRecoverableSession(
	t *testing.T, numHosts int, numInferences int, store storage.Storage,
) ([]types.SlotAssignment, []*signing.Secp256k1Signer, *signing.Secp256k1Signer) {
	t.Helper()
	hosts := make([]*signing.Secp256k1Signer, numHosts)
	for i := range hosts {
		hosts[i] = testutil.MustGenerateKey(t)
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	// Create storage session.
	require.NoError(t, store.CreateSession(storage.CreateSessionParams{
		EscrowID:       "escrow-1",
		CreatorAddr:    user.Address(),
		Config:         config,
		Group:          group,
		InitialBalance: 100000,
	}))

	// Create hosts.
	clients := make([]HostClient, numHosts)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)
		engine := stub.NewInferenceEngine()
		h, err := host.NewHost(sm, hosts[i], engine, "escrow-1", group, nil, host.WithGrace(10))
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	// Create user session with storage.
	userSM := state.NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)
	session, err := NewSession(userSM, user, "escrow-1", group, clients, verifier, WithStorage(store))
	require.NoError(t, err)

	ctx := context.Background()
	params := InferenceParams{
		Model: "llama", Prompt: testutil.TestPrompt,
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}

	for i := 0; i < numInferences; i++ {
		_, err := session.SendInference(ctx, params)
		require.NoError(t, err)
	}

	return group, hosts, user
}

func TestRecoverSession_HappyPath(t *testing.T) {
	store := newTestStore(t)
	numHosts := 3
	numInferences := 5

	group, hosts, user := setupRecoverableSession(t, numHosts, numInferences, store)

	// Build fresh clients for recovery.
	config := testutil.DefaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	clients := make([]HostClient, numHosts)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)
		engine := stub.NewInferenceEngine()
		h, err := host.NewHost(sm, hosts[i], engine, "escrow-1", group, nil, host.WithGrace(10))
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	// Recover.
	session, _, err := RecoverSession(store, user, verifier, "escrow-1", group, clients)
	require.NoError(t, err)
	require.Equal(t, uint64(numInferences), session.Nonce())
	require.Len(t, session.Diffs(), numInferences)

	// Verify can send nonce 6.
	ctx := context.Background()
	params := InferenceParams{
		Model: "llama", Prompt: testutil.TestPrompt,
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	}
	result, err := session.SendInference(ctx, params)
	require.NoError(t, err)
	require.Equal(t, uint64(numInferences+1), result.Nonce)
}

func TestRecoverSession_EmptySession(t *testing.T) {
	store := newTestStore(t)
	hosts := make([]*signing.Secp256k1Signer, 3)
	for i := range hosts {
		hosts[i] = testutil.MustGenerateKey(t)
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(3)
	verifier := signing.NewSecp256k1Verifier()

	require.NoError(t, store.CreateSession(storage.CreateSessionParams{
		EscrowID:       "escrow-1",
		CreatorAddr:    user.Address(),
		Config:         config,
		Group:          group,
		InitialBalance: 100000,
	}))

	clients := make([]HostClient, 3)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)
		h, err := host.NewHost(sm, hosts[i], stub.NewInferenceEngine(), "escrow-1", group, nil)
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	session, _, err := RecoverSession(store, user, verifier, "escrow-1", group, clients)
	require.NoError(t, err)
	require.Equal(t, uint64(0), session.Nonce())
}

func TestRecoverSession_SignaturesRestored(t *testing.T) {
	store := newTestStore(t)
	numHosts := 3
	numInferences := 3

	group, hosts, user := setupRecoverableSession(t, numHosts, numInferences, store)

	config := testutil.DefaultConfig(numHosts)
	verifier := signing.NewSecp256k1Verifier()

	clients := make([]HostClient, numHosts)
	for i := range hosts {
		sm := state.NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)
		h, err := host.NewHost(sm, hosts[i], stub.NewInferenceEngine(), "escrow-1", group, nil, host.WithGrace(10))
		require.NoError(t, err)
		clients[i] = &InProcessClient{Host: h}
	}

	session, _, err := RecoverSession(store, user, verifier, "escrow-1", group, clients)
	require.NoError(t, err)

	// Each inference gets a signature from the executor host.
	sigs := session.Signatures()
	hasSigs := false
	for _, nonceSigs := range sigs {
		if len(nonceSigs) > 0 {
			hasSigs = true
			break
		}
	}
	require.True(t, hasSigs, "recovered session should have signatures")

	// Verify the prompt hash is computed correctly for test data (sanity check).
	_, err = subnet.CanonicalPromptHash(testutil.TestPrompt)
	require.NoError(t, err)
}
