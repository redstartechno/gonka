package gossip

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"subnet/types"
)

// mockPeer records gossip calls.
type mockPeer struct {
	mu          sync.Mutex
	nonceCalls  []nonceCall
	txsCalls    [][]*types.SubnetTx
	nonceCount  atomic.Int32
	failOnNonce bool
}

type nonceCall struct {
	nonce     uint64
	stateHash []byte
	stateSig  []byte
	slotID    uint32
}

func (m *mockPeer) GossipNonce(_ context.Context, nonce uint64, stateHash, stateSig []byte, slotID uint32) error {
	m.nonceCount.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nonceCalls = append(m.nonceCalls, nonceCall{nonce, stateHash, stateSig, slotID})
	if m.failOnNonce {
		return context.DeadlineExceeded
	}
	return nil
}

func (m *mockPeer) GossipTxs(_ context.Context, txs []*types.SubnetTx) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txsCalls = append(m.txsCalls, txs)
	return nil
}

func (m *mockPeer) getNonceCalls() []nonceCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]nonceCall, len(m.nonceCalls))
	copy(result, m.nonceCalls)
	return result
}

// mockMempool records AddTx calls.
type mockMempool struct {
	mu  sync.Mutex
	txs []*types.SubnetTx
}

func (m *mockMempool) AddTx(tx *types.SubnetTx) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.txs = append(m.txs, tx)
}

func TestAfterRequest_SendsToKPeers(t *testing.T) {
	peers := make([]PeerClient, 15)
	mocks := make([]*mockPeer, 15)
	for i := range peers {
		m := &mockPeer{}
		mocks[i] = m
		peers[i] = m
	}

	g := NewGossip("escrow-1", 0, peers, nil)
	g.K = 5

	ctx := context.Background()
	g.AfterRequest(ctx, 1, []byte("hash1"), []byte("sig1"))

	// Exactly K peers should have been contacted.
	total := 0
	for _, m := range mocks {
		total += int(m.nonceCount.Load())
	}
	require.Equal(t, 5, total)
}

func TestAfterRequest_AllPeersWhenLessThanK(t *testing.T) {
	peers := make([]PeerClient, 3)
	mocks := make([]*mockPeer, 3)
	for i := range peers {
		m := &mockPeer{}
		mocks[i] = m
		peers[i] = m
	}

	g := NewGossip("escrow-1", 0, peers, nil)
	g.K = 10

	ctx := context.Background()
	g.AfterRequest(ctx, 1, []byte("hash1"), []byte("sig1"))

	// All 3 peers should be contacted (3 < K=10).
	total := 0
	for _, m := range mocks {
		total += int(m.nonceCount.Load())
	}
	require.Equal(t, 3, total)
}

func TestOnNonceReceived_SameHash_NoError(t *testing.T) {
	g := NewGossip("escrow-1", 0, nil, nil)

	err := g.OnNonceReceived(1, []byte("hash1"), []byte("sig1"), 1)
	require.NoError(t, err)

	err = g.OnNonceReceived(1, []byte("hash1"), []byte("sig2"), 2)
	require.NoError(t, err)
}

func TestOnNonceReceived_DifferentHash_Equivocation(t *testing.T) {
	g := NewGossip("escrow-1", 0, nil, nil)

	err := g.OnNonceReceived(1, []byte("hash1"), []byte("sig1"), 1)
	require.NoError(t, err)

	err = g.OnNonceReceived(1, []byte("hash2"), []byte("sig2"), 2)
	require.Error(t, err)
	require.Contains(t, err.Error(), "equivocation")
}

func TestOnTxsReceived_ForwardsToMempool(t *testing.T) {
	mem := &mockMempool{}
	g := NewGossip("escrow-1", 0, nil, mem)

	txs := []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	}
	g.OnTxsReceived(txs)

	mem.mu.Lock()
	defer mem.mu.Unlock()
	require.Len(t, mem.txs, 1)
}

func TestRebroadcast_StaleUnbackedNonce(t *testing.T) {
	peer := &mockPeer{}
	g := NewGossip("escrow-1", 0, []PeerClient{peer}, nil)
	g.StaleTTL = 10 * time.Millisecond

	// Simulate receiving a nonce that is never backed.
	err := g.OnNonceReceived(5, []byte("hash5"), []byte("sig5"), 2)
	require.NoError(t, err)

	// Wait past StaleTTL.
	time.Sleep(20 * time.Millisecond)

	ctx := context.Background()
	g.rebroadcastStale(ctx)

	calls := peer.getNonceCalls()
	require.Len(t, calls, 1)
	require.Equal(t, uint64(5), calls[0].nonce)
}

func TestRebroadcast_BackedNonce_NotRebroadcast(t *testing.T) {
	peer := &mockPeer{}
	g := NewGossip("escrow-1", 0, []PeerClient{peer}, nil)
	g.StaleTTL = 10 * time.Millisecond

	err := g.OnNonceReceived(5, []byte("hash5"), []byte("sig5"), 2)
	require.NoError(t, err)
	g.MarkBacked(5)

	time.Sleep(20 * time.Millisecond)

	ctx := context.Background()
	g.rebroadcastStale(ctx)

	calls := peer.getNonceCalls()
	require.Len(t, calls, 0, "backed nonce should not be rebroadcast")
}
