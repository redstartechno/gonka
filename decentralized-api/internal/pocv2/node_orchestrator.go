package pocv2

import (
	"context"
	"crypto/sha256"
	"decentralized-api/broker"
	"decentralized-api/chainphase"
	cosmos_client "decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"decentralized-api/mlnodeclient"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/rand"

	"github.com/productscience/inference/x/inference/types"
)

const (
	POC_V2_VALIDATE_BATCH_RETRIES = 5
)

// NodePoCOrchestratorV2 orchestrates PoC v2 validation using artifact-based proofs.
type NodePoCOrchestratorV2 interface {
	ValidateReceivedArtifacts(pocStageStartBlockHeight int64)
}

type NodePoCOrchestratorV2Impl struct {
	pubKey            string
	nodeBroker        *broker.Broker
	callbackUrl       string
	chainBridge       OrchestratorChainBridgeV2
	phaseTracker      *chainphase.ChainPhaseTracker
	offChainValidator *OffChainValidator
}

// OrchestratorChainBridgeV2 provides chain queries for PoC v2.
type OrchestratorChainBridgeV2 interface {
	PoCv2BatchesForStage(startPoCBlockHeight int64) (*PoCBatchesV2Response, error)
	GetBlockHash(height int64) (string, error)
	GetPocParams() (*types.PocParams, error)
}

// PoCBatchesV2Response is the response from querying v2 artifact batches.
// TODO: Replace with types.QueryPocBatchesV2ForStageResponse once query is added to chain.
type PoCBatchesV2Response struct {
	Batches []*PoCBatchesV2ForParticipant
}

// PoCBatchesV2ForParticipant groups artifact batches by participant.
type PoCBatchesV2ForParticipant struct {
	ParticipantAddress string
	PublicKey          string
	Batches            []*PoCBatchV2
}

// PoCBatchV2 represents a single artifact batch.
type PoCBatchV2 struct {
	NodeId    string
	Artifacts []*ArtifactV2
}

// ArtifactV2 represents a single artifact.
type ArtifactV2 struct {
	Nonce     int64
	VectorB64 string
}

type OrchestratorChainBridgeV2Impl struct {
	cosmosClient cosmos_client.CosmosMessageClient
	chainNodeUrl string
}

func (b *OrchestratorChainBridgeV2Impl) PoCv2BatchesForStage(startPoCBlockHeight int64) (*PoCBatchesV2Response, error) {
	// Query the chain for v2 artifact batches
	queryClient := b.cosmosClient.NewInferenceQueryClient()
	resp, err := queryClient.PocV2BatchesForStage(b.cosmosClient.GetContext(), &types.QueryPocV2BatchesForStageRequest{
		BlockHeight: startPoCBlockHeight,
	})
	if err != nil {
		logging.Error("PoCv2BatchesForStage: Failed to query chain", types.PoC,
			"startPoCBlockHeight", startPoCBlockHeight, "error", err)
		return nil, err
	}

	// Transform chain response to orchestrator response format
	result := &PoCBatchesV2Response{
		Batches: make([]*PoCBatchesV2ForParticipant, 0, len(resp.PocBatch)),
	}

	for _, participantBatches := range resp.PocBatch {
		batches := make([]*PoCBatchV2, 0, len(participantBatches.PocBatch))
		for _, chainBatch := range participantBatches.PocBatch {
			artifacts := make([]*ArtifactV2, 0, len(chainBatch.Artifacts))
			for _, artifact := range chainBatch.Artifacts {
				artifacts = append(artifacts, &ArtifactV2{
					Nonce:     int64(artifact.Nonce),
					VectorB64: base64.StdEncoding.EncodeToString(artifact.Vector), // Chain stores as bytes, convert to base64 string
				})
			}
			batches = append(batches, &PoCBatchV2{
				NodeId:    chainBatch.NodeId,
				Artifacts: artifacts,
			})
		}

		result.Batches = append(result.Batches, &PoCBatchesV2ForParticipant{
			ParticipantAddress: participantBatches.Participant,
			PublicKey:          participantBatches.HexPubKey,
			Batches:            batches,
		})
		logging.Info("PoCv2BatchesForStage: Fetched batches from chain", types.PoC, "participant", participantBatches.Participant, "publicKey", participantBatches.HexPubKey, "numBatches", len(batches))
	}

	logging.Info("PoCv2BatchesForStage: Fetched batches from chain", types.PoC,
		"startPoCBlockHeight", startPoCBlockHeight,
		"numParticipants", len(result.Batches))
	return result, nil
}

func (b *OrchestratorChainBridgeV2Impl) GetPocParams() (*types.PocParams, error) {
	response, err := b.cosmosClient.NewInferenceQueryClient().Params(b.cosmosClient.GetContext(), &types.QueryParamsRequest{})
	if err != nil {
		logging.Error("Failed to query params", types.PoC, "error", err)
		return nil, err
	}
	return response.Params.PocParams, nil
}

func (b *OrchestratorChainBridgeV2Impl) GetBlockHash(height int64) (string, error) {
	client, err := cosmos_client.NewRpcClient(b.chainNodeUrl)
	if err != nil {
		return "", err
	}

	block, err := client.Block(context.Background(), &height)
	if err != nil {
		return "", err
	}

	return block.Block.Hash().String(), nil
}

func NewNodePoCOrchestratorV2ForCosmosChain(
	pubKey string,
	nodeBroker *broker.Broker,
	callbackUrl string,
	chainNodeUrl string,
	cosmosClient cosmos_client.CosmosMessageClient,
	phaseTracker *chainphase.ChainPhaseTracker,
) NodePoCOrchestratorV2 {
	return &NodePoCOrchestratorV2Impl{
		pubKey:      pubKey,
		nodeBroker:  nodeBroker,
		callbackUrl: callbackUrl,
		chainBridge: &OrchestratorChainBridgeV2Impl{
			cosmosClient: cosmosClient,
			chainNodeUrl: chainNodeUrl,
		},
		phaseTracker: phaseTracker,
		offChainValidator: NewOffChainValidator(
			cosmosClient,
			nodeBroker,
			phaseTracker,
			callbackUrl,
			pubKey,
			chainNodeUrl,
			DefaultValidationConfig(),
		),
	}
}

func NewNodePoCOrchestratorV2(
	pubKey string,
	nodeBroker *broker.Broker,
	callbackUrl string,
	chainBridge OrchestratorChainBridgeV2,
	phaseTracker *chainphase.ChainPhaseTracker,
) NodePoCOrchestratorV2 {
	return &NodePoCOrchestratorV2Impl{
		pubKey:       pubKey,
		nodeBroker:   nodeBroker,
		callbackUrl:  callbackUrl,
		chainBridge:  chainBridge,
		phaseTracker: phaseTracker,
	}
}

// ValidateReceivedArtifacts validates artifacts from all participants using off-chain proofs.
// It fetches MMR proofs from participant APIs, verifies them, and sends to ML nodes for validation.
func (o *NodePoCOrchestratorV2Impl) ValidateReceivedArtifacts(pocStageStartBlockHeight int64) {
	logging.Info("ValidateReceivedArtifacts (v2): delegating to off-chain validator", types.PoC,
		"pocStageStartBlockHeight", pocStageStartBlockHeight)

	o.offChainValidator.ValidateAll(pocStageStartBlockHeight)
}

// collectUniqueArtifacts collects all unique artifacts from a participant's batches.
func collectUniqueArtifacts(batches *PoCBatchesV2ForParticipant) []mlnodeclient.ArtifactV2 {
	uniqueNonces := make(map[int64]mlnodeclient.ArtifactV2)

	for _, batch := range batches.Batches {
		for _, artifact := range batch.Artifacts {
			if _, exists := uniqueNonces[artifact.Nonce]; !exists {
				uniqueNonces[artifact.Nonce] = mlnodeclient.ArtifactV2{
					Nonce:     artifact.Nonce,
					VectorB64: artifact.VectorB64,
				}
			}
		}
	}

	result := make([]mlnodeclient.ArtifactV2, 0, len(uniqueNonces))
	for _, art := range uniqueNonces {
		result = append(result, art)
	}
	return result
}

// sampleArtifactsV2 samples artifacts deterministically for validation.
func sampleArtifactsV2(
	allArtifacts []mlnodeclient.ArtifactV2,
	validatorPublicKey string,
	blockHash string,
	blockHeight int64,
	nSamples int64,
) []mlnodeclient.ArtifactV2 {
	totalItems := int64(len(allArtifacts))
	if nSamples >= totalItems {
		return allArtifacts
	}

	// Create deterministic indices using same logic as v1
	seedInput := fmt.Sprintf("%s:%s:%d", validatorPublicKey, blockHash, blockHeight)
	hash := sha256.Sum256([]byte(seedInput))
	seed := int64(binary.BigEndian.Uint64(hash[:8]))

	source := rand.NewSource(seed)
	rng := rand.New(source)
	indices := rng.Perm(int(totalItems))[:nSamples]

	sampled := make([]mlnodeclient.ArtifactV2, nSamples)
	for i, idx := range indices {
		sampled[i] = allArtifacts[idx]
	}
	return sampled
}

// filterNodesForV2Validation returns nodes available for PoC v2 validation.
// Relaxed selection criteria for v2:
// - Accept nodes in POC status with any sub-status (generating, validating, idle)
// - Accept nodes in INFERENCE status (they can handle v2 validation requests concurrently)
// - Exclude FAILED or administratively disabled nodes
// This allows testermint and testnet to work even if nodes aren't fully transitioned to POC+VALIDATING.
func filterNodesForV2Validation(nodes []broker.NodeResponse) []broker.NodeResponse {
	filtered := make([]broker.NodeResponse, 0, len(nodes))
	for _, node := range nodes {
		// Exclude failed nodes
		if node.State.CurrentStatus == types.HardwareNodeStatus_FAILED {
			logging.Debug("filterNodesForV2Validation: Skipping FAILED node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Exclude unknown status nodes
		if node.State.CurrentStatus == types.HardwareNodeStatus_UNKNOWN {
			logging.Debug("filterNodesForV2Validation: Skipping UNKNOWN node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Exclude administratively disabled nodes (check AdminState.Enabled)
		if !node.State.AdminState.Enabled {
			logging.Debug("filterNodesForV2Validation: Skipping administratively disabled node", types.PoC, "node_id", node.Node.Id)
			continue
		}

		// Accept nodes in POC status (any sub-status)
		if node.State.CurrentStatus == types.HardwareNodeStatus_POC {
			filtered = append(filtered, node)
			continue
		}

		// Accept nodes in INFERENCE status - v2 validation can run without Stop()
		if node.State.CurrentStatus == types.HardwareNodeStatus_INFERENCE {
			filtered = append(filtered, node)
			continue
		}

		logging.Debug("filterNodesForV2Validation: Skipping node with status", types.PoC,
			"node_id", node.Node.Id, "status", node.State.CurrentStatus.String())
	}
	return filtered
}

// stopGenerationOnAllNodes calls StopPowV2 on all filtered nodes to stop any ongoing generation.
// This is called once at the start of the validation stage transition.
// Errors are logged but do not block validation - we proceed best-effort.
func (o *NodePoCOrchestratorV2Impl) stopGenerationOnAllNodes(nodes []broker.NodeResponse) {
	logging.Info("stopGenerationOnAllNodes: Stopping v2 generation on all nodes before validation", types.PoC,
		"numNodes", len(nodes))

	ctx := context.Background()
	successCount := 0
	failCount := 0

	for _, node := range nodes {
		nodeClient := o.nodeBroker.NewNodeClient(&node.Node)

		_, err := nodeClient.StopPowV2(ctx)
		if err != nil {
			// Log but continue - node might not be generating or might have transient issue
			logging.Warn("stopGenerationOnAllNodes: StopPowV2 failed for node", types.PoC,
				"node_id", node.Node.Id, "host", node.Node.Host, "error", err)
			failCount++
		} else {
			logging.Debug("stopGenerationOnAllNodes: Successfully stopped generation on node", types.PoC,
				"node_id", node.Node.Id, "host", node.Node.Host)
			successCount++
		}
	}

	logging.Info("stopGenerationOnAllNodes: Completed", types.PoC,
		"successCount", successCount, "failCount", failCount)
}
