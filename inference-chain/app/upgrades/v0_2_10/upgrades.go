package v0_2_10

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

func Gonka(amount int64) int64 {
	return amount * 1_000_000_000
}

type BountyReward struct {
	Address string
	Amount  int64
}

var bountyRewards = []BountyReward{
	// Valid fix for minor vulnerability that was previously reported in issue #422
	// PR: https://github.com/gonka-ai/gonka/pull/661
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(500)},

	// Planned task, not a vulnerability, important for the network.
	// PR: https://github.com/gonka-ai/gonka/pull/644
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(700)},

	// Detailed report and fix for a Medium risk vulnerability.
	// PR: https://github.com/gonka-ai/gonka/pull/659
	{Address: "gonka1ejkupq3cy6p8xd64ew2wlzveml86ckpzn9dl56", Amount: Gonka(10000)},

	// First report of the vulnerability fixed in #659
	{Address: "gonka1c34w3r45f0uftjckt2yy4k22vnc3zqjnp0umyz", Amount: Gonka(5000)},

	// Report and fix of low risk vulnerability. Extra appreciation for discovering and
	// reporting it during the review of another PR.
	// PR: https://github.com/gonka-ai/gonka/pull/545
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(1000)},

	// Valid minor bug fix.
	// PR: https://github.com/gonka-ai/gonka/pull/640
	{Address: "gonka1jkydytz99gkh0t42gjj4lz0mmdeumqp7mtzke3", Amount: Gonka(100)},

	// First report and suggested fix. Fixed in PR #661
	// Issue: https://github.com/gonka-ai/gonka/issues/422
	{Address: "gonka123khww9elhtj49zumz0daleaudl6jn9y87tf23", Amount: Gonka(500)},

	// Valid minor bug fix.
	// PR: https://github.com/gonka-ai/gonka/pull/638
	{Address: "gonka1jkydytz99gkh0t42gjj4lz0mmdeumqp7mtzke3", Amount: Gonka(100)},

	// Valid minor bug fix.
	// PR: https://github.com/gonka-ai/gonka/pull/634
	{Address: "gonka1jkydytz99gkh0t42gjj4lz0mmdeumqp7mtzke3", Amount: Gonka(100)},

	// Independent report on the issue addressed by PR #710.
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(5000)},

	// Report and fix of low risk vulnerability.
	// PR: https://github.com/gonka-ai/gonka/pull/643
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(500)},
}

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

		setValidationSlots(ctx, k)
		setPocNormalizationEnabled(ctx, k)
		setPocTimingParams(ctx, k)

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

// setValidationSlots explicitly sets ValidationSlots to 0 (disabled).
// This keeps O(N^2) validation behavior until sampling is enabled via governance.
// Must be enabled only when new participant cost > 0 (see proposals/poc/optimize.md).
func setValidationSlots(ctx context.Context, k keeper.Keeper) {
	params, err := k.GetParams(ctx)
	if err != nil {
		k.LogError("failed to get params during upgrade", types.Upgrades, "error", err)
		return
	}

	if params.PocParams == nil {
		k.LogError("poc params not initialized", types.Upgrades)
		return
	}

	params.PocParams.ValidationSlots = 0

	if err := k.SetParams(ctx, params); err != nil {
		k.LogError("failed to set params with validation slots", types.Upgrades, "error", err)
		return
	}

	k.LogInfo("set validation slots", types.Upgrades, "validation_slots", params.PocParams.ValidationSlots)
}

// setPocNormalizationEnabled explicitly enables time-based weight normalization for PoC.
func setPocNormalizationEnabled(ctx context.Context, k keeper.Keeper) {
	params, err := k.GetParams(ctx)
	if err != nil {
		k.LogError("failed to get params during upgrade", types.Upgrades, "error", err)
		return
	}

	if params.PocParams == nil {
		k.LogError("poc params not initialized", types.Upgrades)
		return
	}

	params.PocParams.PocNormalizationEnabled = true

	if err := k.SetParams(ctx, params); err != nil {
		k.LogError("failed to set params with poc normalization enabled", types.Upgrades, "error", err)
		return
	}

	k.LogInfo("set poc normalization enabled", types.Upgrades, "poc_normalization_enabled", params.PocParams.PocNormalizationEnabled)
}

// setPocTimingParams updates PoC timing parameters:
// - Reduces poc_stage_duration from 60 to 35 blocks
// - Reduces poc_validation_duration from 480 to 240 blocks
// - Scales weight_scale_factor proportionally from 0.262 to 0.449 to maintain same total weight
func setPocTimingParams(ctx context.Context, k keeper.Keeper) {
	params, err := k.GetParams(ctx)
	if err != nil {
		k.LogError("failed to get params during upgrade", types.Upgrades, "error", err)
		return
	}

	if params.EpochParams == nil {
		k.LogError("epoch params not initialized", types.Upgrades)
		return
	}
	if params.PocParams == nil {
		k.LogError("poc params not initialized", types.Upgrades)
		return
	}

	// Update PoC timing: reduce from 60 to 35 blocks
	params.EpochParams.PocStageDuration = 35
	// Update validation duration: reduce from 480 to 240 blocks
	params.EpochParams.PocValidationDuration = 240
	// Scale weight factor proportionally: 0.262 * (60/35) ≈ 0.449
	// Keeps total weight accumulation the same: 0.449 * 35 ≈ 0.262 * 60
	params.PocParams.WeightScaleFactor = &types.Decimal{Value: 449, Exponent: -3}

	if err := k.SetParams(ctx, params); err != nil {
		k.LogError("failed to set poc timing params", types.Upgrades, "error", err)
		return
	}

	k.LogInfo("set poc timing params", types.Upgrades,
		"poc_stage_duration", params.EpochParams.PocStageDuration,
		"poc_validation_duration", params.EpochParams.PocValidationDuration,
		"weight_scale_factor", 0.449)
}

func distributeBountyRewards(ctx context.Context, k keeper.Keeper, distrKeeper distrkeeper.Keeper) error {
	if len(bountyRewards) == 0 {
		k.Logger().Info("No bounty rewards to distribute")
		return nil
	}

	var totalRequired int64
	for _, bounty := range bountyRewards {
		totalRequired += bounty.Amount
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

	for _, bounty := range bountyRewards {
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

		k.Logger().Info("bounty distributed", "address", bounty.Address, "amount", bounty.Amount)
	}

	return nil
}
