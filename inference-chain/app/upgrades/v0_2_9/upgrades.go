package v0_2_9

import (
	"context"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

const preservedModelId = "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"

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
