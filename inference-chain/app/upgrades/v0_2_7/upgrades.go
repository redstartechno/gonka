package v0_2_7

import (
	"context"

	"cosmossdk.io/math"
	upgradetypes "cosmossdk.io/x/upgrade/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

type BountyReward struct {
	Address string
	Amount  int64
}

var (

	// Reward for Epoch 117
	epoch117Rewards = []BountyReward{}

	// Bounty Program
	bountyProgramRewards = []BountyReward{}
)

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
	distrKeeper distrkeeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.Logger().Info("starting upgrade to " + UpgradeName)

		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		if err := setNewPocParams(ctx, k); err != nil {
			return nil, err
		}

		if err := distributeBountyRewards(ctx, k, distrKeeper); err != nil {
			return nil, err
		}

		toVM, err := mm.RunMigrations(ctx, configurator, fromVM)
		if err != nil {
			return toVM, err
		}

		k.Logger().Info("successfully upgraded to " + UpgradeName)
		return toVM, nil
	}
}

func setNewPocParams(ctx context.Context, k keeper.Keeper) error {
	params, err := k.GetParamsSafe(ctx)
	if err != nil {
		return err
	}

	return k.SetParams(ctx, params)
}

func distributeBountyRewards(ctx context.Context, k keeper.Keeper, distrKeeper distrkeeper.Keeper) error {
	sections := []struct {
		name     string
		bounties []BountyReward
	}{
		{"epoch_117", epoch117Rewards},
		{"bounty_program", bountyProgramRewards},
	}

	var totalRequired int64
	for _, section := range sections {
		for _, bounty := range section.bounties {
			totalRequired += bounty.Amount
		}
	}

	feePool, err := distrKeeper.FeePool.Get(ctx)
	if err != nil {
		k.Logger().Warn("failed to get fee pool, skipping bounty distribution", "error", err)
		return nil
	}

	available := feePool.CommunityPool.AmountOf(types.BaseCoin).TruncateInt64()
	if available < totalRequired {
		k.Logger().Warn("insufficient fee pool balance, skipping bounty distribution",
			"required", totalRequired, "available", available)
		return nil
	}

	k.Logger().Info("fee pool balance sufficient", "required", totalRequired, "available", available)

	for _, section := range sections {
		for _, bounty := range section.bounties {
			recipient, err := sdk.AccAddressFromBech32(bounty.Address)
			if err != nil {
				k.Logger().Error("invalid bounty address", "address", bounty.Address, "error", err)
				continue
			}

			coins := sdk.NewCoins(sdk.NewCoin(types.BaseCoin, math.NewInt(bounty.Amount)))
			if err := distrKeeper.DistributeFromFeePool(ctx, coins, recipient); err != nil {
				k.Logger().Error("failed to distribute bounty", "address", bounty.Address, "error", err)
				continue
			}

			k.Logger().Info("bounty distributed", "section", section.name, "address", bounty.Address, "amount", bounty.Amount)
		}
	}

	return nil
}
