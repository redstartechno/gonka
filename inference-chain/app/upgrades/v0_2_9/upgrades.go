package v0_2_9

import (
	"context"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

const preservedModelId = "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"

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
