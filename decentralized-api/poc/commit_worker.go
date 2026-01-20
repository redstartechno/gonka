package poc

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"decentralized-api/chainphase"
	"decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/poc/artifacts"

	"github.com/productscience/inference/api/inference/inference"
	"github.com/productscience/inference/x/inference/types"
)

// commitState tracks the last committed MMR state to avoid duplicate submissions.
type commitState struct {
	count    uint32
	rootHash []byte
}

// CommitWorker owns the entire "artifacts → chain" pipeline:
// - Periodic flush of artifact stores
// - Store commits during generation (time-based, not per-request)
// - Weight distribution at end of generation (state-based for restart robustness)
type CommitWorker struct {
	store              *artifacts.ManagedArtifactStore
	recorder           cosmosclient.CosmosMessageClient
	tracker            *chainphase.ChainPhaseTracker
	participantAddress string

	interval time.Duration
	stop     chan struct{}
	done     chan struct{}

	mu             sync.Mutex
	lastCommitted  map[int64]commitState // pocHeight -> last submitted state
	distributedFor map[int64]bool        // pocHeight -> already distributed locally
}

// NewCommitWorker creates and starts a new commit worker.
// The worker runs until Close() is called.
func NewCommitWorker(
	store *artifacts.ManagedArtifactStore,
	recorder cosmosclient.CosmosMessageClient,
	tracker *chainphase.ChainPhaseTracker,
	participantAddress string,
	interval time.Duration,
) *CommitWorker {
	w := &CommitWorker{
		store:              store,
		recorder:           recorder,
		tracker:            tracker,
		participantAddress: participantAddress,
		interval:           interval,
		stop:               make(chan struct{}),
		done:               make(chan struct{}),
		lastCommitted:      make(map[int64]commitState),
		distributedFor:     make(map[int64]bool),
	}

	// Start flush - always on (same interval as commits)
	store.StartPeriodicFlush(interval)

	go w.run()
	logging.Info("CommitWorker started", types.PoC, "interval", interval)
	return w
}

// Close stops the worker and waits for it to finish.
func (w *CommitWorker) Close() {
	close(w.stop)
	<-w.done
	w.store.StopPeriodicFlush()
	logging.Info("CommitWorker stopped", types.PoC)
}

func (w *CommitWorker) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.tick()
		case <-w.stop:
			return
		}
	}
}

func (w *CommitWorker) tick() {
	epochState := w.tracker.GetCurrentEpochState()
	if epochState == nil || !epochState.IsSynced {
		return
	}

	// V1 mode: CommitWorker is not needed (batches go directly to chain)
	if !epochState.PocV2Enabled {
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	pocHeight := GetCurrentPocStageHeight(epochState)

	// 1. Store Commits
	// Submit commits whenever exchange window is open.
	if pocHeight > 0 {
		canCommit := ShouldAcceptStoreCommit(epochState, pocHeight)
		logging.Debug("CommitWorker: tick", types.PoC,
			"phase", epochState.CurrentPhase,
			"pocHeight", pocHeight,
			"canCommit", canCommit)
		if canCommit {
			w.maybeSubmitCommit(pocHeight)
		}
	}

	// 2. Weight Distribution (State Reconciliation)
	// If we are in a phase where weights SHOULD have been distributed,
	// and we haven't done it for this pocHeight yet, do it now.
	if ShouldHaveDistributedWeights(epochState) && pocHeight > 0 {
		if !w.distributedFor[pocHeight] {
			w.submitWeightDistribution(pocHeight)
		}
	}
}

func (w *CommitWorker) maybeSubmitCommit(pocHeight int64) {
	store, err := w.store.GetStore(pocHeight)
	if err != nil || store == nil {
		logging.Debug("CommitWorker: no store for height", types.PoC, "pocHeight", pocHeight)
		return
	}

	count, rootHash := store.GetFlushedRoot()
	if count == 0 || rootHash == nil {
		logging.Debug("CommitWorker: no flushed data", types.PoC, "pocHeight", pocHeight, "count", count)
		return
	}

	// Skip if unchanged since last commit
	last := w.lastCommitted[pocHeight]
	if last.count == count && bytes.Equal(last.rootHash, rootHash) {
		return
	}

	msg := &inference.MsgPoCV2StoreCommit{
		PocStageStartBlockHeight: pocHeight,
		Count:                    count,
		RootHash:                 rootHash,
	}

	if err := w.recorder.SubmitPoCV2StoreCommit(msg); err != nil {
		logging.Warn("CommitWorker: commit failed", types.PoC,
			"pocHeight", pocHeight, "error", err)
		return
	}

	w.lastCommitted[pocHeight] = commitState{count, rootHash}
	logging.Debug("CommitWorker: committed", types.PoC,
		"pocHeight", pocHeight, "count", count)
}

func (w *CommitWorker) submitWeightDistribution(pocHeight int64) {
	if w.distributedFor[pocHeight] {
		return
	}

	store, err := w.store.GetStore(pocHeight)
	if err != nil || store == nil {
		return
	}

	// 1. Query chain for the canonical committed snapshot
	if w.participantAddress == "" {
		logging.Warn("CommitWorker: no participant address, skipping distribution", types.PoC)
		return
	}

	queryClient := w.recorder.NewInferenceQueryClient()
	resp, err := queryClient.PoCV2StoreCommit(context.Background(), &types.QueryPoCV2StoreCommitRequest{
		PocStageStartBlockHeight: pocHeight,
		ParticipantAddress:       w.participantAddress,
	})
	if err != nil {
		logging.Warn("CommitWorker: failed to query last commit", types.PoC,
			"pocHeight", pocHeight, "error", err)
		return
	}
	if !resp.Found || resp.Count == 0 {
		logging.Debug("CommitWorker: no committed snapshot found, skipping distribution", types.PoC,
			"pocHeight", pocHeight)
		return
	}

	// 2. Flush local store to ensure all data is persisted
	if err := store.Flush(); err != nil {
		logging.Warn("CommitWorker: flush failed before distribution", types.PoC,
			"pocHeight", pocHeight, "error", err)
	}

	// 3. Get local distribution
	distribution := store.GetNodeDistribution()
	if len(distribution) == 0 {
		logging.Debug("CommitWorker: empty distribution", types.PoC, "pocHeight", pocHeight)
		return
	}

	// 4. Build weights adjusted to match committed count
	weights, err := getWeightDistribution(distribution, resp.Count)
	if err != nil {
		logging.Error("CommitWorker: failed to build weight distribution", types.PoC,
			"pocHeight", pocHeight, "error", err)
		return
	}

	msg := &inference.MsgMLNodeWeightDistribution{
		PocStageStartBlockHeight: pocHeight,
		Weights:                  weights,
	}

	if err := w.recorder.SubmitMLNodeWeightDistribution(msg); err != nil {
		logging.Warn("CommitWorker: distribution failed", types.PoC,
			"pocHeight", pocHeight, "error", err)
		return
	}

	w.distributedFor[pocHeight] = true
	logging.Info("CommitWorker: distributed weights", types.PoC,
		"pocHeight", pocHeight, "nodes", len(weights), "count", resp.Count)
}

// getWeightDistribution builds weight distribution adjusted to sum exactly to targetCount.
func getWeightDistribution(distribution map[string]uint32, targetCount uint32) ([]*inference.MLNodeWeight, error) {
	if len(distribution) == 0 {
		return nil, fmt.Errorf("empty distribution")
	}
	if targetCount == 0 {
		return nil, fmt.Errorf("targetCount is 0")
	}

	// Calculate local sum
	var localSum uint32
	for _, count := range distribution {
		localSum += count
	}

	if localSum == 0 {
		return nil, fmt.Errorf("distribution sum is 0")
	}

	// If sums match, no adjustment needed
	if localSum == targetCount {
		weights := make([]*inference.MLNodeWeight, 0, len(distribution))
		for nodeId, count := range distribution {
			weights = append(weights, &inference.MLNodeWeight{
				NodeId: nodeId,
				Weight: count,
			})
		}
		return weights, nil
	}

	// Proportionally adjust weights to match targetCount
	logging.Warn("CommitWorker: adjusting distribution proportionally", types.PoC,
		"localSum", localSum, "targetCount", targetCount)

	ratio := float64(targetCount) / float64(localSum)

	type nodeWeight struct {
		nodeId    string
		weight    uint32
		remainder float64
	}
	nodeWeights := make([]nodeWeight, 0, len(distribution))
	var adjustedSum uint32

	for nodeId, count := range distribution {
		exact := float64(count) * ratio
		rounded := uint32(exact)
		remainder := exact - float64(rounded)
		nodeWeights = append(nodeWeights, nodeWeight{nodeId, rounded, remainder})
		adjustedSum += rounded
	}

	// Distribute remaining count to nodes with highest remainders
	diff := int32(targetCount) - int32(adjustedSum)
	if diff > 0 {
		for i := 0; i < int(diff); i++ {
			maxIdx := 0
			maxRem := nodeWeights[0].remainder
			for j := 1; j < len(nodeWeights); j++ {
				if nodeWeights[j].remainder > maxRem {
					maxIdx = j
					maxRem = nodeWeights[j].remainder
				}
			}
			nodeWeights[maxIdx].weight++
			nodeWeights[maxIdx].remainder = -1 // Mark as used
		}
	}

	weights := make([]*inference.MLNodeWeight, 0, len(nodeWeights))
	for _, nw := range nodeWeights {
		if nw.weight > 0 {
			weights = append(weights, &inference.MLNodeWeight{
				NodeId: nw.nodeId,
				Weight: nw.weight,
			})
		}
	}

	return weights, nil
}
