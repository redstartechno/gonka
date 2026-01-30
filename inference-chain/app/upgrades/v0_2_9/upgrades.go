package v0_2_9

import (
	"context"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

const preservedModelId = "Qwen/Qwen3-4B-Instruct-2507"

// allowedTransferAgents is the list of bech32 addresses allowed to act as Transfer Agents.
// TODO: Fill in the actual addresses before deploying the upgrade.
var allowedTransferAgents = []string{
	// "inference1...",
	// "inference1...",
}

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.Logger().Info("starting upgrade to " + UpgradeName)

		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		removeNonQwen235BModels(ctx, k)
		setAllowedTransferAgents(ctx, k)
		resetPocSlotsForEffectiveEpoch(ctx, k)
		resetPocSlotsInEpochGroupData(ctx, k)

		toVM, err := mm.RunMigrations(ctx, configurator, fromVM)
		if err != nil {
			return toVM, err
		}

		k.Logger().Info("successfully upgraded to " + UpgradeName)
		return toVM, nil
	}
}

func removeNonQwen235BModels(ctx context.Context, k keeper.Keeper) {
	models, err := k.GetGovernanceModels(ctx)
	if err != nil {
		k.LogError("failed to get governance models during upgrade", types.Upgrades, "error", err)
		return
	}

	for _, model := range models {
		if model.Id != preservedModelId {
			k.DeleteGovernanceModel(ctx, model.Id)
			k.LogInfo("removed governance model", types.Upgrades, "model_id", model.Id)
		}
	}
}

func setAllowedTransferAgents(ctx context.Context, k keeper.Keeper) {
	if len(allowedTransferAgents) == 0 {
		k.LogInfo("no allowed transfer agents configured, skipping TA whitelist setup", types.Upgrades)
		return
	}

	params, err := k.GetParams(ctx)
	if err != nil {
		k.LogError("failed to get params during upgrade", types.Upgrades, "error", err)
		return
	}

	params.TransferAgentAccessParams = &types.TransferAgentAccessParams{
		AllowedTransferAddresses: allowedTransferAgents,
	}

	if err := k.SetParams(ctx, params); err != nil {
		k.LogError("failed to set params with transfer agent whitelist", types.Upgrades, "error", err)
		return
	}

	k.LogInfo("set allowed transfer agents", types.Upgrades, "count", len(allowedTransferAgents))
}

// resetPocSlotsForUpcomingEpoch clears POC_SLOT=true allocations for all nodes in the upcoming epoch.
//
// # Background
//
// Each epoch has a POC (Proof of Compute) phase at its beginning where nodes prove their compute capacity.
// To maintain network availability during POC, some nodes are allocated to continue serving inference
// instead of participating in POC. This is controlled by the TimeslotAllocation field in MLNodeInfo:
//
//	TimeslotAllocation[0] = PRE_POC_SLOT (always true for active nodes)
//	TimeslotAllocation[1] = POC_SLOT (true = serve inference during POC, false = participate in POC)
//
// # Data Structures
//
//	ActiveParticipants (stored per epoch):
//	  └── Participants []*ActiveParticipant
//	        └── MlNodes []*ModelMLNodes
//	              └── MlNodes []*MLNodeInfo
//	                    └── TimeslotAllocation []bool  <-- We reset index [1] to false
//
// # Why This Reset is Needed
//
// When enabling POC V2, we want ALL nodes to participate in POC at the start of the first V2 epoch.
// This ensures:
//  1. Fresh POC data from all nodes for the new V2 system
//  2. No nodes carry over preserved weight without proving their compute
//
// # Timing
//
// This runs during the upgrade, which happens in the inference phase of epoch A:
//
//	Epoch A: [PocStart, PocEnd, SetNewValidators, ...upgrade HERE..., NextPocStart]
//	                              ↓                       ↓                 ↓
//	                    ActiveParticipants(A)      Reset POC_SLOT      POC A+1 reads
//	                    created at PocEnd          in upgrade          ActiveParticipants(A)
//
// During POC A+1, GetPreservedNodesByParticipant reads ActiveParticipants(A) to determine
// which nodes have POC_SLOT=true and should preserve their weight. By resetting
// ActiveParticipants(A) here, all nodes will participate in POC A+1.
func resetPocSlotsForEffectiveEpoch(ctx context.Context, k keeper.Keeper) {
	effectiveEpochIndex, found := k.GetEffectiveEpochIndex(ctx)
	if !found {
		k.LogWarn("resetPocSlotsForEffectiveEpoch: no effective epoch found, skipping", types.Upgrades)
		return
	}

	participants, found := k.GetActiveParticipants(ctx, effectiveEpochIndex)
	if !found {
		k.LogWarn("resetPocSlotsForEffectiveEpoch: no active participants for effective epoch", types.Upgrades,
			"epoch", effectiveEpochIndex)
		return
	}

	resetCount := 0
	for _, p := range participants.Participants {
		for _, modelMLNodes := range p.MlNodes {
			if modelMLNodes == nil {
				continue
			}
			for _, mlNode := range modelMLNodes.MlNodes {
				if mlNode == nil {
					continue
				}
				// TimeslotAllocation[1] is POC_SLOT: true means node serves inference during POC
				// We set it to false so all nodes participate in POC
				if len(mlNode.TimeslotAllocation) > 1 && mlNode.TimeslotAllocation[1] {
					mlNode.TimeslotAllocation[1] = false
					resetCount++
				}
			}
		}
	}

	if resetCount > 0 {
		if err := k.SetActiveParticipants(ctx, participants); err != nil {
			k.LogError("resetPocSlotsForEffectiveEpoch: failed to save reset allocations", types.Upgrades,
				"error", err)
			return
		}
		k.LogInfo("resetPocSlotsForEffectiveEpoch: reset POC_SLOT allocations for first V2 epoch", types.Upgrades,
			"epoch", effectiveEpochIndex, "nodes_reset", resetCount)
	} else {
		k.LogInfo("resetPocSlotsForEffectiveEpoch: no POC_SLOT allocations to reset", types.Upgrades,
			"epoch", effectiveEpochIndex)
	}
}

// resetPocSlotsInEpochGroupData resets POC_SLOT=true allocations in EpochGroupData for all model subgroups.
//
// # Why This is Needed
//
// The broker reads TimeslotAllocation from EpochGroupData (not ActiveParticipants) to determine
// which nodes should continue serving inference during POC via ShouldContinueInference().
// EpochGroupData is created at EndOfPoCValidationStage BEFORE the upgrade runs, so we must
// also reset it here to ensure the broker sees the correct values.
//
// # Data Structures
//
//	EpochGroupData (stored per epoch + model):
//	  └── ValidationWeights []*ValidationWeight
//	        └── MlNodes []*MLNodeInfo
//	              └── TimeslotAllocation []bool  <-- We reset index [1] to false
//
// Note: The parent EpochGroupData (modelId="") has no MlNodes, only model subgroups do.
func resetPocSlotsInEpochGroupData(ctx context.Context, k keeper.Keeper) {
	effectiveEpochIndex, found := k.GetEffectiveEpochIndex(ctx)
	if !found {
		k.LogWarn("resetPocSlotsInEpochGroupData: no effective epoch found, skipping", types.Upgrades)
		return
	}

	// Get parent EpochGroupData to find all model subgroups
	parentData, found := k.GetEpochGroupData(ctx, effectiveEpochIndex, "")
	if !found {
		k.LogWarn("resetPocSlotsInEpochGroupData: parent epoch group data not found", types.Upgrades,
			"epoch", effectiveEpochIndex)
		return
	}

	totalResetCount := 0

	// Reset each model subgroup (parent has no MlNodes, only subgroups do)
	for _, modelId := range parentData.SubGroupModels {
		subgroupData, found := k.GetEpochGroupData(ctx, effectiveEpochIndex, modelId)
		if !found {
			k.LogWarn("resetPocSlotsInEpochGroupData: subgroup not found", types.Upgrades,
				"epoch", effectiveEpochIndex, "model", modelId)
			continue
		}

		resetCount := 0
		for _, vw := range subgroupData.ValidationWeights {
			if vw == nil {
				continue
			}
			for _, mlNode := range vw.MlNodes {
				if mlNode == nil {
					continue
				}
				// TimeslotAllocation[1] is POC_SLOT: true means node serves inference during POC
				// We set it to false so all nodes participate in POC
				if len(mlNode.TimeslotAllocation) > 1 && mlNode.TimeslotAllocation[1] {
					mlNode.TimeslotAllocation[1] = false
					resetCount++
				}
			}
		}

		if resetCount > 0 {
			k.SetEpochGroupData(ctx, subgroupData)
			totalResetCount += resetCount
			k.LogInfo("resetPocSlotsInEpochGroupData: reset POC_SLOT in subgroup", types.Upgrades,
				"epoch", effectiveEpochIndex, "model", modelId, "nodes_reset", resetCount)
		}
	}

	if totalResetCount > 0 {
		k.LogInfo("resetPocSlotsInEpochGroupData: reset POC_SLOT in EpochGroupData complete", types.Upgrades,
			"epoch", effectiveEpochIndex, "total_nodes_reset", totalResetCount)
	} else {
		k.LogInfo("resetPocSlotsInEpochGroupData: no POC_SLOT allocations to reset in EpochGroupData", types.Upgrades,
			"epoch", effectiveEpochIndex)
	}
}
