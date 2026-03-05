package gossip

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"subnet/logging"
	"subnet/types"
)

// nonceRecord tracks state hash seen at a given nonce.
type nonceRecord struct {
	stateHash   []byte
	stateSig    []byte
	slotID      uint32
	seenAt      time.Time
	rebroadcast bool // true after first rebroadcast
}

// Gossip propagates nonce notifications and detects equivocation.
type Gossip struct {
	mu       sync.Mutex
	escrowID string
	slotID   uint32
	peers    []PeerClient
	seen     map[uint64]*nonceRecord
	K        int           // fanout, default 10
	StaleTTL time.Duration // how long unapplied nonces stay before re-gossip

	highestSeen uint64 // tracked O(1)

	mempool          MempoolSink    // receives forwarded txs
	sigAccumulator   SigAccumulator // receives sigs for applied nonces
	diffFetcher      DiffFetcher    // fetches diffs from peers for recovery
	stateUpdater     StateUpdater   // applies recovered diffs
	RecoveryDelay    time.Duration  // delay before recovery triggers (default 60s)
	RecoveryTick     time.Duration  // recovery loop interval (default 60s)
	lastAfterReq     time.Time      // last time AfterRequest was called
	lastAfterReqNonce uint64        // nonce from the most recent AfterRequest call

	broadcastedTxs map[uint64]bool // hash of proto bytes -> already sent

	stopCh    chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once
}

// NewGossip creates a new gossip instance.
func NewGossip(escrowID string, slotID uint32, peers []PeerClient, mempool MempoolSink) *Gossip {
	return &Gossip{
		escrowID:       escrowID,
		slotID:         slotID,
		peers:          peers,
		seen:           make(map[uint64]*nonceRecord),
		K:              10,
		StaleTTL:       120 * time.Second,
		RecoveryDelay:  60 * time.Second,
		RecoveryTick:   60 * time.Second,
		mempool:        mempool,
		broadcastedTxs: make(map[uint64]bool),
		stopCh:         make(chan struct{}),
		stopped:        make(chan struct{}),
	}
}

// SetSigAccumulator sets the callback for accumulating gossip signatures.
func (g *Gossip) SetSigAccumulator(acc SigAccumulator) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sigAccumulator = acc
}

// SetRecovery configures the recovery dependencies.
func (g *Gossip) SetRecovery(fetcher DiffFetcher, updater StateUpdater) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.diffFetcher = fetcher
	g.stateUpdater = updater
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
	}
	if nonce > g.highestSeen {
		g.highestSeen = nonce
	}
	g.lastAfterReq = time.Now()
	g.lastAfterReqNonce = nonce
	peers := g.pickPeers()
	g.mu.Unlock()

	g.sendNonceToPeers(ctx, peers, nonce, stateHash, stateSig, g.slotID)
}

// OnNonceReceived handles incoming nonce notifications from peers.
// Returns an error if equivocation is detected (same nonce, different hash).
func (g *Gossip) OnNonceReceived(nonce uint64, stateHash, stateSig []byte, senderSlot uint32) error {
	g.mu.Lock()

	existing, ok := g.seen[nonce]
	if ok {
		if !bytes.Equal(existing.stateHash, stateHash) {
			g.mu.Unlock()
			return fmt.Errorf("equivocation at nonce %d: hash %x vs %x (slots %d vs %d)",
				nonce, existing.stateHash, stateHash, existing.slotID, senderSlot)
		}
		// Already seen with same hash. Try to accumulate signature.
		if g.sigAccumulator != nil {
			acc := g.sigAccumulator
			g.mu.Unlock()
			if err := acc.AccumulateGossipSig(nonce, stateHash, stateSig, senderSlot); err != nil {
				logging.Debug("accumulate gossip sig failed", "subsystem", "gossip", "nonce", nonce, "error", err)
			}
			return nil
		}
		g.mu.Unlock()
		return nil
	}

	g.seen[nonce] = &nonceRecord{
		stateHash: stateHash,
		stateSig:  stateSig,
		slotID:    senderSlot,
		seenAt:    time.Now(),
	}
	if nonce > g.highestSeen {
		g.highestSeen = nonce
	}
	peers := g.pickPeers()
	g.mu.Unlock()

	// Amplification: forward new nonce to K random peers.
	go g.sendNonceToPeers(context.Background(), peers, nonce, stateHash, stateSig, senderSlot)

	return nil
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

// HighestSeen returns the highest nonce seen via gossip or AfterRequest.
func (g *Gossip) HighestSeen() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.highestSeen
}

// PruneBelow removes seen-map entries below nonce - margin.
// Called by the host after successful finalization. The 100-nonce margin
// keeps a window for late-arriving equivocation evidence.
func (g *Gossip) PruneBelow(nonce uint64) {
	const margin = 100
	g.mu.Lock()
	defer g.mu.Unlock()

	if nonce <= margin {
		return
	}
	cutoff := nonce - margin
	for n := range g.seen {
		if n < cutoff {
			delete(g.seen, n)
		}
	}

	// Cap dedup map to prevent unbounded growth. Clearing causes at most
	// one redundant broadcast per tx, which is harmless.
	const maxBroadcastedTxs = 10000
	if len(g.broadcastedTxs) > maxBroadcastedTxs {
		clear(g.broadcastedTxs)
	}
}

// BroadcastTxs sends txs to ALL peers with dedup.
// Stale txs are rare and critical, so we broadcast to everyone (not K random).
func (g *Gossip) BroadcastTxs(ctx context.Context, txs []*types.SubnetTx) {
	if len(txs) == 0 {
		return
	}

	g.mu.Lock()
	var newTxs []*types.SubnetTx
	for _, tx := range txs {
		h := txHash(tx)
		if !g.broadcastedTxs[h] {
			g.broadcastedTxs[h] = true
			newTxs = append(newTxs, tx)
		}
	}
	peers := make([]PeerClient, len(g.peers))
	copy(peers, g.peers)
	g.mu.Unlock()

	if len(newTxs) == 0 {
		return
	}

	var wg sync.WaitGroup
	for _, p := range peers {
		wg.Add(1)
		go func(peer PeerClient) {
			defer wg.Done()
			sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := peer.GossipTxs(sendCtx, newTxs); err != nil {
				logging.Debug("broadcast txs failed", "subsystem", "gossip", "error", err)
			}
		}(p)
	}
	wg.Wait()
}

// txHash returns a deterministic hash for dedup. Uses sha256 of proto bytes.
func txHash(tx *types.SubnetTx) uint64 {
	data, err := proto.Marshal(tx)
	if err != nil {
		return 0
	}
	h := sha256.Sum256(data)
	// Use first 8 bytes as uint64.
	var v uint64
	for i := 0; i < 8; i++ {
		v = (v << 8) | uint64(h[i])
	}
	return v
}

// Start begins the background re-propagation and recovery loops.
func (g *Gossip) Start(ctx context.Context) {
	go g.backgroundLoop(ctx)
}

// Stop halts the background loop. Safe to call multiple times.
func (g *Gossip) Stop() {
	g.closeOnce.Do(func() {
		close(g.stopCh)
	})
	<-g.stopped
}

func (g *Gossip) backgroundLoop(ctx context.Context) {
	defer close(g.stopped)
	rebroadcastTicker := time.NewTicker(30 * time.Second)
	defer rebroadcastTicker.Stop()

	recoveryTick := g.RecoveryTick
	if recoveryTick <= 0 {
		recoveryTick = 60 * time.Second
	}
	recoveryTicker := time.NewTicker(recoveryTick)
	defer recoveryTicker.Stop()

	for {
		select {
		case <-g.stopCh:
			return
		case <-ctx.Done():
			return
		case <-rebroadcastTicker.C:
			g.rebroadcastStale(ctx)
		case <-recoveryTicker.C:
			g.tryRecovery(ctx)
		}
	}
}

func (g *Gossip) rebroadcastStale(ctx context.Context) {
	g.mu.Lock()
	now := time.Now()
	var stale []nonceRecord
	var staleNonces []uint64
	for nonce, rec := range g.seen {
		if rec.rebroadcast {
			continue
		}
		if now.Sub(rec.seenAt) > g.StaleTTL {
			stale = append(stale, *rec)
			staleNonces = append(staleNonces, nonce)
			rec.rebroadcast = true
		}
	}
	peers := g.pickPeers()
	g.mu.Unlock()

	for i, rec := range stale {
		logging.Debug("re-gossip stale nonce", "subsystem", "gossip", "nonce", staleNonces[i])
		g.sendNonceToPeers(ctx, peers, staleNonces[i], rec.stateHash, rec.stateSig, rec.slotID)
	}
}

func (g *Gossip) tryRecovery(ctx context.Context) {
	g.mu.Lock()
	fetcher := g.diffFetcher
	updater := g.stateUpdater
	lastReq := g.lastAfterReq
	recoveryDelay := g.RecoveryDelay
	highestSeen := g.highestSeen
	lastAppliedNonce := g.lastAfterReqNonce
	g.mu.Unlock()

	if fetcher == nil || updater == nil {
		return
	}

	if highestSeen <= lastAppliedNonce {
		return
	}

	// Only trigger recovery if we haven't received a user request recently.
	if !lastReq.IsZero() && time.Since(lastReq) < recoveryDelay {
		return
	}

	logging.Debug("gossip recovery triggered",
		"subsystem", "gossip",
		"highest_seen", highestSeen,
		"last_after_req_nonce", lastAppliedNonce,
	)

	diffs, err := fetcher.GetDiffs(ctx, lastAppliedNonce+1, highestSeen)
	if err != nil {
		logging.Debug("recovery fetch diffs failed", "subsystem", "gossip", "error", err)
		return
	}
	if len(diffs) == 0 {
		return
	}

	sigs, err := updater.ApplyRecoveredDiffs(ctx, diffs)
	if err != nil {
		logging.Debug("recovery apply diffs failed", "subsystem", "gossip", "error", err)
		return
	}

	// Update watermark to highest recovered nonce.
	var maxRecovered uint64
	for _, sig := range sigs {
		if sig.Nonce > maxRecovered {
			maxRecovered = sig.Nonce
		}
	}

	// Ensure recovered nonces are in the seen map and gossip own sigs.
	g.mu.Lock()
	for _, sig := range sigs {
		if _, ok := g.seen[sig.Nonce]; !ok {
			g.seen[sig.Nonce] = &nonceRecord{
				stateHash: sig.StateHash,
				stateSig:  sig.Sig,
				slotID:    sig.SlotID,
				seenAt:    time.Now(),
			}
		}
		if sig.Nonce > g.highestSeen {
			g.highestSeen = sig.Nonce
		}
	}
	if maxRecovered > g.lastAfterReqNonce {
		g.lastAfterReqNonce = maxRecovered
		g.lastAfterReq = time.Now()
	}
	peers := g.pickPeers()
	g.mu.Unlock()

	for _, sig := range sigs {
		g.sendNonceToPeers(ctx, peers, sig.Nonce, sig.StateHash, sig.Sig, sig.SlotID)
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
