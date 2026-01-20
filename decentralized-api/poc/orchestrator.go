package poc

import (
	"decentralized-api/broker"
	"decentralized-api/chainphase"
	cosmosclient "decentralized-api/cosmosclient"
	"decentralized-api/logging"

	"github.com/productscience/inference/x/inference/types"
)

// Orchestrator is the interface for PoC orchestration.
// Kept as a potential switch point for V1/V2 migration (see migration.md).
type Orchestrator interface {
	ValidateReceivedArtifacts(pocStageStartBlockHeight int64)
}

// orchestratorImpl is the minimal PoC v2 orchestrator.
// It delegates to OffChainValidator for artifact validation.
type orchestratorImpl struct {
	validator *OffChainValidator
}

// NewOrchestrator creates a new PoC orchestrator.
func NewOrchestrator(
	pubKey string,
	nodeBroker *broker.Broker,
	callbackUrl string,
	chainNodeUrl string,
	cosmosClient cosmosclient.CosmosMessageClient,
	phaseTracker *chainphase.ChainPhaseTracker,
) Orchestrator {
	return &orchestratorImpl{
		validator: NewOffChainValidator(
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

// ValidateReceivedArtifacts validates artifacts for the given PoC stage.
func (o *orchestratorImpl) ValidateReceivedArtifacts(pocStageStartBlockHeight int64) {
	logging.Info("Orchestrator: delegating to off-chain validator", types.PoC,
		"pocStageStartBlockHeight", pocStageStartBlockHeight)

	o.validator.ValidateAll(pocStageStartBlockHeight)
}
