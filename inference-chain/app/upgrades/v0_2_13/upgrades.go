package v0_2_13

import (
	"context"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

const MaxEscrowsPerEpoch uint32 = 500_000

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, _ upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.LogInfo("starting upgrade", types.Upgrades, "version", UpgradeName)

		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		if err := setDevshardMaxEscrowsPerEpoch(ctx, k); err != nil {
			return nil, err
		}

		toVM, err := mm.RunMigrations(ctx, configurator, fromVM)
		if err != nil {
			return toVM, err
		}

		k.LogInfo("successfully upgraded", types.Upgrades, "version", UpgradeName)
		return toVM, nil
	}
}

func setDevshardMaxEscrowsPerEpoch(ctx context.Context, k keeper.Keeper) error {
	params, err := k.GetParams(ctx)
	if err != nil {
		return err
	}
	if params.DevshardEscrowParams == nil {
		params.DevshardEscrowParams = types.DefaultDevshardEscrowParams()
	}
	params.DevshardEscrowParams.MaxEscrowsPerEpoch = MaxEscrowsPerEpoch
	if err := k.SetParams(ctx, params); err != nil {
		return err
	}
	k.LogInfo("set devshard max_escrows_per_epoch", types.Upgrades,
		"max_escrows_per_epoch", MaxEscrowsPerEpoch)
	return nil
}
