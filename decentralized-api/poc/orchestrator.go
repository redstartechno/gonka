package poc

import (
	"decentralized-api/broker"
	"decentralized-api/chainphase"
	cosmosclient "decentralized-api/cosmosclient"
	"decentralized-api/logging"

	"github.com/productscience/inference/x/inference/types"
)

// Orchestrator is the interface for PoC orchestration.
// Supports V1/V2 dispatch based on poc_v2_enabled governance parameter.
type Orchestrator interface {
	ValidateReceivedArtifacts(pocStageStartBlockHeight int64, pocStartBlockHash string)
}

// orchestratorImpl coordinates PoC validation with V1/V2 dispatch.
type orchestratorImpl struct {
	validatorV1    *OnChainValidator  // V1: queries chain for PoCBatch
	validatorV2    *OffChainValidator // V2: fetches proofs from participant APIs
	isPoCv2Enabled func() bool        // version check function
}

// NewOrchestrator creates a new PoC orchestrator with V1/V2 validators.
func NewOrchestrator(
	pubKey string,
	nodeBroker *broker.Broker,
	callbackUrl string,
	chainNodeUrl string,
	cosmosClient cosmosclient.CosmosMessageClient,
	phaseTracker *chainphase.ChainPhaseTracker,
) Orchestrator {
	config := DefaultValidationConfig()

	// V2 validator (off-chain proofs)
	validatorV2 := NewOffChainValidator(
		cosmosClient,
		nodeBroker,
		phaseTracker,
		callbackUrl,
		pubKey,
		chainNodeUrl,
		config,
	)

	// V1 validator (on-chain batches)
	validatorV1 := NewOnChainValidator(
		cosmosClient,
		nodeBroker,
		phaseTracker,
		callbackUrl,
		pubKey,
		chainNodeUrl,
		config,
	)

	// Version check via phaseTracker
	isPoCv2Enabled := func() bool {
		if phaseTracker == nil {
			return true // default V2
		}
		return phaseTracker.IsPoCv2Enabled()
	}

	return &orchestratorImpl{
		validatorV1:    validatorV1,
		validatorV2:    validatorV2,
		isPoCv2Enabled: isPoCv2Enabled,
	}
}

// ValidateReceivedArtifacts validates artifacts for the given PoC stage.
// Dispatches to V1 or V2 validator based on poc_v2_enabled governance parameter.
// pocStartBlockHash is the block hash at PoC start - must match the hash used during generation.
func (o *orchestratorImpl) ValidateReceivedArtifacts(pocStageStartBlockHeight int64, pocStartBlockHash string) {
	if o.isPoCv2Enabled() {
		logging.Info("Orchestrator: delegating to V2 off-chain validator", types.PoC,
			"pocStageStartBlockHeight", pocStageStartBlockHeight,
			"pocStartBlockHash", pocStartBlockHash)
		o.validatorV2.ValidateAll(pocStageStartBlockHeight, pocStartBlockHash)
		return
	}

	logging.Info("Orchestrator: delegating to V1 on-chain validator", types.PoC,
		"pocStageStartBlockHeight", pocStageStartBlockHeight)
	o.validatorV1.ValidateAll(pocStageStartBlockHeight, pocStartBlockHash)
}
