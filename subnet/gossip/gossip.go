package gossip

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"subnet/logging"
	"subnet/types"
)

// nonceRecord tracks state hash seen at a given nonce.
type nonceRecord struct {
	stateHash []byte
	stateSig  []byte
	slotID    uint32
	seenAt    time.Time
	backed    bool // true if a diff at this nonce was applied
}

// Gossip propagates nonce notifications and detects equivocation.
type Gossip struct {
	mu       sync.Mutex
	escrowID string
	slotID   uint32
	peers    []PeerClient
	seen     map[uint64]*nonceRecord
	K        int           // fanout, default 10
	StaleTTL time.Duration // how long unbacked nonces stay before re-gossip
	mempool  MempoolSink   // receives forwarded txs
	stopCh   chan struct{}
	stopped  chan struct{}
}

// MempoolSink receives transactions from gossip peers.
type MempoolSink interface {
	AddTx(tx *types.SubnetTx)
}

// NewGossip creates a new gossip instance.
func NewGossip(escrowID string, slotID uint32, peers []PeerClient, mempool MempoolSink) *Gossip {
	return &Gossip{
		escrowID: escrowID,
		slotID:   slotID,
		peers:    peers,
		seen:     make(map[uint64]*nonceRecord),
		K:        10,
		StaleTTL: 120 * time.Second,
		mempool:  mempool,
		stopCh:   make(chan struct{}),
		stopped:  make(chan struct{}),
	}
}

// AfterRequest is called after a successful host request.
// It propagates the nonce+stateHash to K random peers.
func (g *Gossip) AfterRequest(ctx context.Context, nonce uint64, stateHash, stateSig []byte) {
	g.mu.Lock()
	g.seen[nonce] = &nonceRecord{
		stateHash: stateHash,
		stateSig:  stateSig,
		slotID:    g.slotID,
		seenAt:    time.Now(),
		backed:    true, // we applied the diff, so it's backed
	}
	peers := g.pickPeers()
	g.mu.Unlock()

	g.sendNonceToPeers(ctx, peers, nonce, stateHash, stateSig, g.slotID)
}

// OnNonceReceived handles incoming nonce notifications from peers.
// Returns an error if equivocation is detected (same nonce, different hash).
func (g *Gossip) OnNonceReceived(nonce uint64, stateHash, stateSig []byte, senderSlot uint32) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	existing, ok := g.seen[nonce]
	if ok {
		if !bytesEqual(existing.stateHash, stateHash) {
			return fmt.Errorf("equivocation at nonce %d: hash %x vs %x (slots %d vs %d)",
				nonce, existing.stateHash, stateHash, existing.slotID, senderSlot)
		}
		return nil
	}

	g.seen[nonce] = &nonceRecord{
		stateHash: stateHash,
		stateSig:  stateSig,
		slotID:    senderSlot,
		seenAt:    time.Now(),
	}
	return nil
}

// MarkBacked marks a nonce as backed by a diff (applied locally).
func (g *Gossip) MarkBacked(nonce uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rec, ok := g.seen[nonce]; ok {
		rec.backed = true
	}
}

// OnTxsReceived handles incoming transactions from peers.
func (g *Gossip) OnTxsReceived(txs []*types.SubnetTx) {
	if g.mempool == nil {
		return
	}
	for _, tx := range txs {
		g.mempool.AddTx(tx)
	}
}

// Start begins the background re-propagation loop.
func (g *Gossip) Start(ctx context.Context) {
	go g.rebroadcastLoop(ctx)
}

// Stop halts the background loop.
func (g *Gossip) Stop() {
	close(g.stopCh)
	<-g.stopped
}

func (g *Gossip) rebroadcastLoop(ctx context.Context) {
	defer close(g.stopped)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.rebroadcastStale(ctx)
		}
	}
}

func (g *Gossip) rebroadcastStale(ctx context.Context) {
	g.mu.Lock()
	now := time.Now()
	var stale []nonceRecord
	var staleNonces []uint64
	for nonce, rec := range g.seen {
		if !rec.backed && now.Sub(rec.seenAt) > g.StaleTTL {
			stale = append(stale, *rec)
			staleNonces = append(staleNonces, nonce)
		}
	}
	peers := g.pickPeers()
	g.mu.Unlock()

	for i, rec := range stale {
		logging.Debug("re-gossip stale nonce", "subsystem", "gossip", "nonce", staleNonces[i])
		g.sendNonceToPeers(ctx, peers, staleNonces[i], rec.stateHash, rec.stateSig, rec.slotID)
	}
}

func (g *Gossip) pickPeers() []PeerClient {
	if len(g.peers) <= g.K {
		result := make([]PeerClient, len(g.peers))
		copy(result, g.peers)
		return result
	}
	indices := rand.Perm(len(g.peers))[:g.K]
	result := make([]PeerClient, len(indices))
	for i, idx := range indices {
		result[i] = g.peers[idx]
	}
	return result
}

func (g *Gossip) sendNonceToPeers(ctx context.Context, peers []PeerClient, nonce uint64, stateHash, stateSig []byte, slotID uint32) {
	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(peer PeerClient) {
			defer wg.Done()
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := peer.GossipNonce(sendCtx, nonce, stateHash, stateSig, slotID); err != nil {
				logging.Debug("gossip nonce send failed", "subsystem", "gossip", "nonce", nonce, "error", err)
			}
		}(p)
	}
	wg.Wait()
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
