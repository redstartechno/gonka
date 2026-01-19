package pocv2

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"

	"decentralized-api/broker"
	"decentralized-api/chainphase"
	"decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/pocartifacts"

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
	store    *pocartifacts.ManagedArtifactStore
	recorder cosmosclient.CosmosMessageClient
	tracker  *chainphase.ChainPhaseTracker
	broker   *broker.Broker

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
	store *pocartifacts.ManagedArtifactStore,
	recorder cosmosclient.CosmosMessageClient,
	tracker *chainphase.ChainPhaseTracker,
	broker *broker.Broker,
	interval time.Duration,
) *CommitWorker {
	w := &CommitWorker{
		store:          store,
		recorder:       recorder,
		tracker:        tracker,
		broker:         broker,
		interval:       interval,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lastCommitted:  make(map[int64]commitState),
		distributedFor: make(map[int64]bool),
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

	w.mu.Lock()
	defer w.mu.Unlock()

	inGeneration := w.broker.IsInPoCGeneratePhase()
	pocHeight := w.getPocStageHeight(epochState)

	// 1. Weight Distribution (State Reconciliation)
	// If we are in a phase where weights SHOULD have been distributed (Validation/WindDown),
	// and we haven't done it for this pocHeight yet, do it now.
	// This handles restarts gracefully and fixes the confirmation PoC bug.
	if w.shouldHaveDistributedWeights(epochState) && pocHeight > 0 {
		if !w.distributedFor[pocHeight] {
			w.submitWeightDistribution(pocHeight)
		}
	}

	// 2. Store Commits
	// During generation, periodically commit state if changed.
	// Must be window-aware (keeper rejects after exchange closes).
	if inGeneration && pocHeight > 0 && w.shouldAcceptStoreCommit(epochState, pocHeight) {
		w.maybeSubmitCommit(pocHeight)
	}
}

// getPocStageHeight returns the correct PoC stage height based on context.
// For regular PoC: PocStartBlockHeight
// For confirmation PoC: TriggerHeight
func (w *CommitWorker) getPocStageHeight(epochState *chainphase.EpochState) int64 {
	if epochState.IsNilOrNotSynced() {
		return 0
	}

	// Confirmation PoC uses event's trigger height
	if epochState.ActiveConfirmationPoCEvent != nil &&
		epochState.CurrentPhase == types.InferencePhase {
		return epochState.ActiveConfirmationPoCEvent.TriggerHeight
	}

	// Regular PoC
	return epochState.LatestEpoch.PocStartBlockHeight
}

// shouldAcceptStoreCommit returns true if the chain will accept MsgPoCV2StoreCommit
// at the current block height. Mirrors keeper validation.
func (w *CommitWorker) shouldAcceptStoreCommit(epochState *chainphase.EpochState, pocStageStartHeight int64) bool {
	if epochState.IsNilOrNotSynced() {
		return false
	}

	currentHeight := epochState.CurrentBlock.Height

	// Confirmation PoC: check batch submission window
	if epochState.ActiveConfirmationPoCEvent != nil &&
		epochState.CurrentPhase == types.InferencePhase {
		event := epochState.ActiveConfirmationPoCEvent
		if event.Phase != types.ConfirmationPoCPhase_CONFIRMATION_POC_GENERATION {
			return false
		}
		if pocStageStartHeight != event.TriggerHeight {
			return false
		}
		epochParams := &epochState.LatestEpoch.EpochParams
		return event.IsInBatchSubmissionWindow(currentHeight, epochParams)
	}

	// Regular PoC: check exchange window
	if epochState.CurrentPhase != types.PoCGeneratePhase &&
		epochState.CurrentPhase != types.PoCGenerateWindDownPhase {
		return false
	}

	if !epochState.LatestEpoch.IsStartOfPocStage(pocStageStartHeight) {
		return false
	}

	return epochState.LatestEpoch.IsPoCExchangeWindow(currentHeight)
}

// shouldHaveDistributedWeights returns true if we should be in a state where weights
// have been distributed (Validation phase or WindDown phase).
func (w *CommitWorker) shouldHaveDistributedWeights(epochState *chainphase.EpochState) bool {
	if epochState.IsNilOrNotSynced() {
		return false
	}

	// Regular PoC: Validation or WindDown phases
	if epochState.CurrentPhase == types.PoCValidatePhase ||
		epochState.CurrentPhase == types.PoCValidateWindDownPhase ||
		epochState.CurrentPhase == types.PoCGenerateWindDownPhase {
		return true
	}

	// Confirmation PoC: Validation phase
	if epochState.CurrentPhase == types.InferencePhase &&
		epochState.ActiveConfirmationPoCEvent != nil &&
		epochState.ActiveConfirmationPoCEvent.Phase == types.ConfirmationPoCPhase_CONFIRMATION_POC_VALIDATION {
		return true
	}

	return false
}

func (w *CommitWorker) maybeSubmitCommit(pocHeight int64) {
	store, err := w.store.GetStore(pocHeight)
	if err != nil || store == nil {
		return
	}

	count, rootHash := store.GetFlushedRoot()
	if count == 0 || rootHash == nil {
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
	// This ensures we distribute for exactly what was accepted on-chain
	participantAddress := w.broker.GetParticipantAddress()
	if participantAddress == "" {
		logging.Warn("CommitWorker: no participant address, skipping distribution", types.PoC)
		return
	}

	queryClient := w.recorder.NewInferenceQueryClient()
	resp, err := queryClient.PoCV2StoreCommit(context.Background(), &types.QueryPoCV2StoreCommitRequest{
		PocStageStartBlockHeight: pocHeight,
		ParticipantAddress:       participantAddress,
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
// If local distribution differs from target, weights are proportionally scaled.
// Preconditions: distribution must be non-empty, targetCount must be > 0.
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

	// Use float64 for proportional calculation, then round
	ratio := float64(targetCount) / float64(localSum)

	// First pass: calculate proportional weights
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

	// Second pass: distribute remaining count to nodes with highest remainders
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
