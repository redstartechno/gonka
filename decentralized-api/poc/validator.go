package poc

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"decentralized-api/broker"
	"decentralized-api/chainphase"
	"decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/mlnodeclient"

	"github.com/productscience/inference/x/inference/types"
)

// OffChainValidator handles off-chain PoC validation using MMR proofs.
type OffChainValidator struct {
	recorder     cosmosclient.CosmosMessageClient
	nodeBroker   *broker.Broker
	phaseTracker *chainphase.ChainPhaseTracker
	callbackUrl  string
	pubKey       string
	chainNodeUrl string

	config ValidationConfig
}

// ValidationConfig contains configuration for off-chain validation.
type ValidationConfig struct {
	WorkerCount    int
	RequestTimeout time.Duration
	MaxRetries     int
	RetryBackoff   time.Duration
}

// DefaultValidationConfig returns the default configuration.
func DefaultValidationConfig() ValidationConfig {
	return ValidationConfig{
		WorkerCount:    10,
		RequestTimeout: 30 * time.Second,
		MaxRetries:     3,
		RetryBackoff:   5 * time.Second,
	}
}

// validateResult represents the outcome of validating a participant.
type validateResult int

const (
	validateSuccess       validateResult = iota // Validation succeeded
	validateFailPermanent                       // Permanent failure (fraud, invalid proof) - no retry
	validateFailRetry                           // Transient failure (network, ML node) - can retry
)

// participantWork represents a single participant to validate.
type participantWork struct {
	address  string
	url      string
	pubKey   string
	count    uint32
	rootHash []byte
	attempt  int // current attempt number (0-based)
}

// NewOffChainValidator creates a new off-chain validator.
func NewOffChainValidator(
	recorder cosmosclient.CosmosMessageClient,
	nodeBroker *broker.Broker,
	phaseTracker *chainphase.ChainPhaseTracker,
	callbackUrl string,
	pubKey string,
	chainNodeUrl string,
	config ValidationConfig,
) *OffChainValidator {
	return &OffChainValidator{
		recorder:     recorder,
		nodeBroker:   nodeBroker,
		phaseTracker: phaseTracker,
		callbackUrl:  callbackUrl,
		pubKey:       pubKey,
		chainNodeUrl: chainNodeUrl,
		config:       config,
	}
}

// ValidateAll validates all participants who committed artifacts for the given PoC stage.
func (v *OffChainValidator) ValidateAll(pocStageStartBlockHeight int64) {
	logging.Info("OffChainValidator: starting validation", types.PoC,
		"pocStageStartBlockHeight", pocStageStartBlockHeight)

	epochState := v.phaseTracker.GetCurrentEpochState()
	if epochState == nil {
		logging.Error("OffChainValidator: epoch state is nil", types.PoC)
		return
	}

	// Get block hash for sampling randomness
	blockHash := v.getBlockHash(epochState, pocStageStartBlockHeight)
	if blockHash == "" {
		logging.Error("OffChainValidator: failed to get block hash", types.PoC)
		return
	}

	// Get PoC params
	queryClient := v.recorder.NewInferenceQueryClient()
	paramsResp, err := queryClient.Params(context.Background(), &types.QueryParamsRequest{})
	if err != nil {
		logging.Error("OffChainValidator: failed to get params", types.PoC, "error", err)
		return
	}
	pocParams := paramsResp.Params.PocParams
	sampleSize := int(pocParams.ValidationSampleSize)
	if sampleSize == 0 {
		sampleSize = 200
	}

	// Get available ML nodes for validation
	nodes, err := v.nodeBroker.GetNodes()
	if err != nil {
		logging.Error("OffChainValidator: failed to get nodes", types.PoC, "error", err)
		return
	}
	nodes = filterNodesForValidation(nodes)
	if len(nodes) == 0 {
		logging.Error("OffChainValidator: no nodes available", types.PoC)
		return
	}

	// Stop generation on all nodes before validation
	v.stopGenerationOnAllNodes(nodes)

	// Query all store commits for this stage
	commitsResp, err := queryClient.AllPoCV2StoreCommitsForStage(context.Background(),
		&types.QueryAllPoCV2StoreCommitsForStageRequest{
			PocStageStartBlockHeight: pocStageStartBlockHeight,
		})
	if err != nil {
		logging.Error("OffChainValidator: failed to query commits", types.PoC, "error", err)
		return
	}

	if len(commitsResp.Commits) == 0 {
		logging.Info("OffChainValidator: no commits found for stage", types.PoC,
			"pocStageStartBlockHeight", pocStageStartBlockHeight)
		return
	}

	logging.Info("OffChainValidator: found participants with commits", types.PoC,
		"count", len(commitsResp.Commits))

	// Build work items with participant URLs
	workItems := make([]participantWork, 0, len(commitsResp.Commits))
	for _, commit := range commitsResp.Commits {
		// Get participant's inference URL
		participantResp, err := queryClient.Participant(context.Background(),
			&types.QueryGetParticipantRequest{Index: commit.ParticipantAddress})
		if err != nil {
			logging.Warn("OffChainValidator: failed to get participant", types.PoC,
				"address", commit.ParticipantAddress, "error", err)
			continue
		}

		if participantResp.Participant.InferenceUrl == "" {
			logging.Warn("OffChainValidator: participant has no URL", types.PoC,
				"address", commit.ParticipantAddress)
			continue
		}

		// Get participant's public key for ML node (from commit query)
		if commit.HexPubKey == "" {
			logging.Warn("OffChainValidator: participant has no public key", types.PoC,
				"address", commit.ParticipantAddress)
			continue
		}

		workItems = append(workItems, participantWork{
			address:  commit.ParticipantAddress,
			url:      participantResp.Participant.InferenceUrl,
			pubKey:   commit.HexPubKey,
			count:    commit.Count,
			rootHash: commit.RootHash,
		})
	}

	if len(workItems) == 0 {
		logging.Warn("OffChainValidator: no valid work items", types.PoC)
		return
	}

	// Randomize order to avoid thundering herd
	rand.Shuffle(len(workItems), func(i, j int) {
		workItems[i], workItems[j] = workItems[j], workItems[i]
	})

	// Create proof client
	proofClient := NewProofClient(v.recorder, ProofClientConfig{Timeout: v.config.RequestTimeout})

	// Create work channel - buffered to allow re-queueing failed items
	// Size: initial items + potential retries
	workChan := make(chan participantWork, len(workItems)*2)
	var wg sync.WaitGroup

	// Track statistics
	var statsMu sync.Mutex
	successCount := 0
	failCount := 0
	pendingCount := len(workItems)

	// Context for coordinating shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start workers
	numWorkers := v.config.WorkerCount
	if numWorkers > len(workItems) {
		numWorkers = len(workItems)
	}
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			v.worker(
				ctx,
				workerID,
				workChan,
				proofClient,
				nodes,
				pocStageStartBlockHeight,
				blockHash,
				pocParams,
				sampleSize,
				&statsMu,
				&successCount,
				&failCount,
				&pendingCount,
			)
		}(i)
	}

	// Send initial work items
	for _, item := range workItems {
		workChan <- item
	}

	// Wait for all workers to complete
	wg.Wait()
	close(workChan)

	logging.Info("OffChainValidator: validation complete", types.PoC,
		"pocStageStartBlockHeight", pocStageStartBlockHeight,
		"totalParticipants", len(workItems),
		"successful", successCount,
		"failed", failCount)
}

// worker processes participants from the work channel.
// Failed items are re-queued for retry instead of blocking on retries.
func (v *OffChainValidator) worker(
	ctx context.Context,
	workerID int,
	workChan chan participantWork,
	proofClient *ProofClient,
	nodes []broker.NodeResponse,
	pocHeight int64,
	blockHash string,
	pocParams *types.PocParams,
	sampleSize int,
	statsMu *sync.Mutex,
	successCount *int,
	failCount *int,
	pendingCount *int,
) {
	nodeCounter := workerID // Start from different nodes per worker

	for {
		select {
		case <-ctx.Done():
			return
		case work, ok := <-workChan:
			if !ok {
				return
			}

			result := v.validateParticipant(
				workerID,
				work,
				proofClient,
				nodes,
				&nodeCounter,
				pocHeight,
				blockHash,
				pocParams,
				sampleSize,
			)

			statsMu.Lock()
			switch result {
			case validateSuccess:
				*successCount++
				*pendingCount--
			case validateFailPermanent:
				*failCount++
				*pendingCount--
			case validateFailRetry:
				// Re-queue for retry if under max attempts
				if work.attempt < v.config.MaxRetries-1 {
					work.attempt++
					// Non-blocking send - if channel is full, count as failed
					select {
					case workChan <- work:
						logging.Debug("OffChainValidator: re-queued for retry", types.PoC,
							"participant", work.address, "attempt", work.attempt)
					default:
						*failCount++
						*pendingCount--
						logging.Warn("OffChainValidator: queue full, marking as failed", types.PoC,
							"participant", work.address)
					}
				} else {
					*failCount++
					*pendingCount--
					logging.Warn("OffChainValidator: max retries exceeded", types.PoC,
						"participant", work.address, "attempts", work.attempt+1)
				}
			}

			// Check if all work is done
			done := *pendingCount <= 0
			statsMu.Unlock()

			if done {
				return
			}
		}
	}
}

// validateParticipant validates a single participant.
// Returns validateResult indicating success, permanent failure, or retryable failure.
func (v *OffChainValidator) validateParticipant(
	workerID int,
	work participantWork,
	proofClient *ProofClient,
	nodes []broker.NodeResponse,
	nodeCounter *int,
	pocHeight int64,
	blockHash string,
	pocParams *types.PocParams,
	sampleSize int,
) validateResult {
	ctx := context.Background()

	logging.Debug("OffChainValidator: validating participant", types.PoC,
		"worker", workerID, "participant", work.address, "count", work.count, "attempt", work.attempt)

	// Sample leaf indices
	leafIndices := sampleLeafIndices(v.pubKey, blockHash, pocHeight, work.count, sampleSize)

	// Fetch and verify proofs
	verified, err := proofClient.FetchAndVerifyProofs(ctx, work.url, ProofRequest{
		PocStageStartBlockHeight: pocHeight,
		RootHash:                 work.rootHash,
		Count:                    work.count,
		LeafIndices:              leafIndices,
		ParticipantAddress:       work.address,
	})
	if err != nil {
		logging.Warn("OffChainValidator: proof fetch/verify failed", types.PoC,
			"participant", work.address, "attempt", work.attempt, "error", err)
		// Proof verification failures are permanent - no point retrying
		if errors.Is(err, ErrProofVerificationFailed) {
			return validateFailPermanent
		}
		// Transient error (network/timeout) - retry
		return validateFailRetry
	}

	// Check for duplicate nonces (fraud) - permanent failure
	if err := CheckDuplicateNonces(verified); err != nil {
		logging.Warn("OffChainValidator: duplicate nonces detected (fraud)", types.PoC,
			"participant", work.address, "error", err)
		return validateFailPermanent
	}

	// Convert verified artifacts to ML node format
	artifacts := make([]mlnodeclient.ArtifactV2, len(verified))
	nonces := make([]int64, len(verified))
	for i, a := range verified {
		artifacts[i] = mlnodeclient.ArtifactV2{
			Nonce:     int64(a.Nonce),
			VectorB64: a.VectorB64,
		}
		nonces[i] = int64(a.Nonce)
	}

	// Send to ML node for statistical validation
	validationCallbackUrl := v.callbackUrl + "/v2/poc-batches"
	validationReq := mlnodeclient.PoCGenerateRequestV2{
		BlockHash:   blockHash,
		BlockHeight: pocHeight,
		PublicKey:   work.pubKey,
		NodeCount:   len(nodes),
		Nonces:      nonces,
		Params: mlnodeclient.PoCParamsV2{
			Model:  pocParams.ModelId,
			SeqLen: pocParams.SeqLen,
		},
		URL: validationCallbackUrl,
		Validation: &mlnodeclient.ValidationV2{
			Artifacts: artifacts,
		},
		StatTest: mlnodeclient.DefaultStatTestParamsV2(),
	}

	// Try sending to ML node (single attempt per call - retries handled by queue)
	node := nodes[*nodeCounter%len(nodes)]
	*nodeCounter++

	validationReq.NodeId = int(node.Node.NodeNum)

	nodeClient := v.nodeBroker.NewNodeClient(&node.Node)
	_, err = nodeClient.GenerateV2(ctx, validationReq)
	if err == nil {
		logging.Debug("OffChainValidator: sent to ML node", types.PoC,
			"participant", work.address, "node", node.Node.Host)
		return validateSuccess
	}

	logging.Warn("OffChainValidator: ML node request failed", types.PoC,
		"participant", work.address, "node", node.Node.Host, "attempt", work.attempt, "error", err)
	return validateFailRetry
}

// sampleLeafIndices generates deterministic leaf indices for sampling.
func sampleLeafIndices(validatorPubKey string, blockHash string, blockHeight int64, count uint32, sampleSize int) []uint32 {
	if count == 0 {
		return nil
	}

	n := int(count)
	if sampleSize >= n {
		// Return all indices
		indices := make([]uint32, n)
		for i := 0; i < n; i++ {
			indices[i] = uint32(i)
		}
		return indices
	}

	// Create deterministic seed
	seedInput := fmt.Sprintf("%s:%s:%d", validatorPubKey, blockHash, blockHeight)
	hash := sha256.Sum256([]byte(seedInput))
	seed := int64(binary.BigEndian.Uint64(hash[:8]))

	source := rand.NewSource(seed)
	rng := rand.New(source)

	// Sample without replacement using Fisher-Yates partial shuffle
	indices := make([]uint32, n)
	for i := 0; i < n; i++ {
		indices[i] = uint32(i)
	}

	result := make([]uint32, sampleSize)
	for i := 0; i < sampleSize; i++ {
		j := i + rng.Intn(n-i)
		indices[i], indices[j] = indices[j], indices[i]
		result[i] = indices[i]
	}

	return result
}

// getBlockHash returns the block hash for sampling randomness.
func (v *OffChainValidator) getBlockHash(epochState *chainphase.EpochState, pocStageStartBlockHeight int64) string {
	// Use current block hash if available
	if epochState.CurrentBlock.Hash != "" {
		return epochState.CurrentBlock.Hash
	}

	// For confirmation PoC, use event hash
	if epochState.CurrentPhase == types.InferencePhase && epochState.ActiveConfirmationPoCEvent != nil {
		return epochState.ActiveConfirmationPoCEvent.PocSeedBlockHash
	}

	// Query block hash from chain
	if v.chainNodeUrl == "" {
		logging.Warn("OffChainValidator: no chain node URL, using empty hash", types.PoC)
		return ""
	}

	client, err := cosmosclient.NewRpcClient(v.chainNodeUrl)
	if err != nil {
		logging.Error("OffChainValidator: failed to create RPC client", types.PoC, "error", err)
		return ""
	}

	block, err := client.Block(context.Background(), &pocStageStartBlockHeight)
	if err != nil {
		logging.Error("OffChainValidator: failed to get block", types.PoC, "error", err)
		return ""
	}

	return block.Block.Hash().String()
}

// stopGenerationOnAllNodes stops PoC generation on all nodes.
func (v *OffChainValidator) stopGenerationOnAllNodes(nodes []broker.NodeResponse) {
	logging.Info("OffChainValidator: stopping generation on all nodes", types.PoC,
		"numNodes", len(nodes))

	ctx := context.Background()
	successCount := 0
	failCount := 0

	for _, node := range nodes {
		nodeClient := v.nodeBroker.NewNodeClient(&node.Node)
		_, err := nodeClient.StopPowV2(ctx)
		if err != nil {
			logging.Warn("OffChainValidator: StopPowV2 failed", types.PoC,
				"node", node.Node.Host, "error", err)
			failCount++
		} else {
			successCount++
		}
	}

	logging.Info("OffChainValidator: stop generation complete", types.PoC,
		"success", successCount, "failed", failCount)
}

// filterNodesForValidation returns nodes available for PoC validation.
// - Accept nodes in POC status with any sub-status
// - Accept nodes in INFERENCE status
// - Exclude FAILED or administratively disabled nodes
func filterNodesForValidation(nodes []broker.NodeResponse) []broker.NodeResponse {
	filtered := make([]broker.NodeResponse, 0, len(nodes))
	for _, node := range nodes {
		// Exclude failed nodes
		if node.State.CurrentStatus == types.HardwareNodeStatus_FAILED {
			logging.Debug("filterNodesForValidation: Skipping FAILED node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Exclude unknown status nodes
		if node.State.CurrentStatus == types.HardwareNodeStatus_UNKNOWN {
			logging.Debug("filterNodesForValidation: Skipping UNKNOWN node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Exclude administratively disabled nodes
		if !node.State.AdminState.Enabled {
			logging.Debug("filterNodesForValidation: Skipping administratively disabled node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Accept nodes in POC status (any sub-status)
		if node.State.CurrentStatus == types.HardwareNodeStatus_POC {
			filtered = append(filtered, node)
			continue
		}

		// Accept nodes in INFERENCE status
		if node.State.CurrentStatus == types.HardwareNodeStatus_INFERENCE {
			filtered = append(filtered, node)
			continue
		}

		logging.Debug("filterNodesForValidation: Skipping node with status", types.PoC,
			"node_id", node.Node.Id, "status", node.State.CurrentStatus.String())
	}
	return filtered
}
