package v0_2_8

import (
	"context"
	"errors"
	"fmt"

	"cosmossdk.io/math"
	upgradetypes "cosmossdk.io/x/upgrade/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	distrkeeper "github.com/cosmos/cosmos-sdk/x/distribution/keeper"
	blskeeper "github.com/productscience/inference/x/bls/keeper"
	blstypes "github.com/productscience/inference/x/bls/types"
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
	// Perf: optimize participants endpoint with single balance query
	// - Optimizes the `/v1/participants` endpoint by replacing N gRPC calls with a single blockchain query.
	// - Achieves ~500x speedup for large sets of participants.
	// PR: https://github.com/gonka-ai/gonka/pull/536
	// @x0152
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(500)},
	// Security: prevent SSRF via executor redirect
	// - Prevents SSRF attacks where a malicious executor redirects Transfer Agent requests to internal services (e.g., admin API).
	// - Implements a custom HTTP client that disables following redirects.
	// PR: https://github.com/gonka-ai/gonka/pull/534
	// @x0152
	{Address: "gonka18enyz7h6hh5zjveee5wnhkhrcexamfz0zdxxqe", Amount: Gonka(500)},
	// Security Fixes for v0.2.7:
	// - SSRF & DoS:** Validates `InferenceUrl` to reject internal IPs and adds timeouts to prevent request hangs.
	// - Vote Flipping:** Prevents overwriting of PoC validations by rejecting duplicates.
	// - Batch Size Limits:** Enforces bounds on PoC batch sizes to prevent state bloat.
	// - PoC Exclusion:** Fixes `getInferenceServingNodeIds` to correctly exclude inference-serving nodes.
	// PR: https://github.com/gonka-ai/gonka/pull/505
	// @ouicate
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(12500)},
	// Auth Bypass & Replay: Binds `epochId` to signatures and validates authorization against the correct epoch.
	// PR: https://github.com/gonka-ai/gonka/commit/8853af800a88c170d06f560e8a1a28de9c45ea61
	// @ouicate
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(5000)},
	// Request timestamp is in the future leads to missed inferences for hosts
	// PR: https://github.com/gonka-ai/gonka/issues/518
	// @akup
	{Address: "gonka1ejkupq3cy6p8xd64ew2wlzveml86ckpzn9dl56", Amount: Gonka(2000)},
	// Fix(bls): reject duplicate slot indices in partial signatures
	// - Rejects partial signatures with duplicate slot indices to prevent verification failures during aggregation.
	// PR: https://github.com/gonka-ai/gonka/pull/551
	// @yapion
	{Address: "gonka1s8szs7n43jxgz4a4xaxmzm5emh7fmjxhach7w8", Amount: Gonka(5000)},
	// Non Determinism in denom, included in 0.2.7
	// PR: https://github.com/gonka-ai/gonka/commit/a0cdbf64f6ac05f86f9edede1770c614a4cfc228
	// @ouicate
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(11988)},
	// CICD Vulnerability, included in 0.2.7
	// PR: https://github.com/gonka-ai/gonka/pull/509
	// @ouicate
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(2498)},
	// Low-VRAM GPUs, included in 0.2.7
	{Address: "gonka1wpan224906ant68frjd8vqreaxr87hudy2wvd9", Amount: Gonka(3497)},
	// vLLM 0.11.0 — Migration Proposal
	// PR: https://github.com/gonka-ai/gonka/issues/647
	// @Axel-T
	{Address: "gonka1yhdhp4vwsvdsplv4acksntx0zxh8saueq6lj9m", Amount: Gonka(10000)},
	// Inference: defense-in-depth against int overflow
	// - Fixes integer overflow vulnerabilities in escrow and cost calculations using checked arithmetic.
	// - Adds hard caps for token counts and improves error handling to fail closed on overflows.
	// PR: https://github.com/gonka-ai/gonka/pull/544
	// @ouicate
	{Address: "gonka1f0elpwnx7ezytdlck35003nz6qk8kzvurvnj4a", Amount: Gonka(25000)},
	// Fix(inference): update totalDistributed after debt deduction
	// - Fixes a bug where `totalDistributed` was not updated after deducting debt, causing tokens to be lost instead of returned to governance.
	// PR: https://github.com/gonka-ai/gonka/pull/607
	// @yapion
	{Address: "gonka1s8szs7n43jxgz4a4xaxmzm5emh7fmjxhach7w8", Amount: Gonka(500)},
}

// Total: 78983 GONKA across 12 rewards

func CreateUpgradeHandler(
	mm *module.Manager,
	configurator module.Configurator,
	k keeper.Keeper,
	bk blskeeper.Keeper,
	distrKeeper distrkeeper.Keeper,
) upgradetypes.UpgradeHandler {
	return func(ctx context.Context, plan upgradetypes.Plan, fromVM module.VersionMap) (module.VersionMap, error) {
		k.Logger().Info("starting upgrade to " + UpgradeName)

		if _, ok := fromVM["capability"]; !ok {
			fromVM["capability"] = mm.Modules["capability"].(module.HasConsensusVersion).ConsensusVersion()
		}

		err := burnExtraCommunityCoins(ctx, &k)
		if err != nil {
			k.LogError("Error removing community account", types.Tokenomics, "err", err)
		}

		if err := MigrateBLSData(ctx, k, bk); err != nil {
			k.LogError("Error precomputing slot public keys", types.Tokenomics, "err", err)
			return nil, err
		}

		if err := setV0_2_8Params(ctx, k); err != nil {
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

func burnExtraCommunityCoins(ctx context.Context, k *keeper.Keeper) error {
	// This account and it's coins were inadvertently created during genesis. The coins are NOT
	// part of the economic plan for Gonka. The actual community pool coins will not be impacted.
	const moduleName = "pre_programmed_sale"
	expectedAddr := "gonka1rmac644w5hjsyxfggz6e4empxf02vegkt3ppec"

	actualAddr := k.AccountKeeper.GetModuleAddress(moduleName)
	if actualAddr == nil {
		return fmt.Errorf("module account '%s' does not exist", moduleName)
	}

	actualBech32 := actualAddr.String()
	if actualBech32 != expectedAddr {
		return fmt.Errorf("module account address mismatch: expected %s, got %s", expectedAddr, actualBech32)
	}

	coins := k.BankView.SpendableCoins(ctx, actualAddr)
	if coins.IsZero() {
		k.LogInfo("No coins to burn in 'pre_programmed_sale' account", types.Tokenomics, "coins", coins)
		return nil
	}

	err := k.BankKeeper.BurnCoins(ctx, moduleName, coins, "one-time burn of pre_programmed_sale account")
	if err != nil {
		return fmt.Errorf("failed to burn coins: %w", err)
	}

	k.LogInfo("Successfully burned all coins from 'pre_programmed_sale'", types.Tokenomics, "coins", coins)
	return nil
}

func MigrateBLSData(ctx context.Context, k keeper.Keeper, bk blskeeper.Keeper) error {
	sdkCtx := sdk.UnwrapSDKContext(ctx)

	// Get the currently effective epoch ID from the inference module.
	// This is the epoch currently responsible for validation and threshold signing.
	epochID, found := k.GetEffectiveEpochIndex(sdkCtx)
	if !found {
		bk.Logger().Info("No effective epoch found during upgrade")
		return nil
	}

	bk.Logger().Info("Checking BLS data migration for current epoch", "epoch_id", epochID)
	epochData, err := bk.GetEpochBLSData(sdkCtx, epochID)
	if err != nil {
		if errors.Is(err, blstypes.ErrEpochBLSDataNotFound) {
			bk.Logger().Info("Epoch BLS data not found", "epoch_id", epochID)
			return nil
		}
		return fmt.Errorf("failed to get epoch %d data: %w", epochID, err)
	}

	if epochData.DkgPhase == blstypes.DKGPhase_DKG_PHASE_COMPLETED || epochData.DkgPhase == blstypes.DKGPhase_DKG_PHASE_SIGNED {
		if len(epochData.SlotPublicKeys) == 0 {
			bk.Logger().Info("Generating precomputed slot public keys for epoch", "epoch_id", epochID)
			slotKeys, err := bk.PrecomputeSlotPublicKeysBlst(&epochData)
			if err != nil {
				return fmt.Errorf("failed to precompute slot keys for epoch %d: %w", epochID, err)
			}
			epochData.SlotPublicKeys = slotKeys
			if err := bk.SetEpochBLSData(sdkCtx, epochData); err != nil {
				return fmt.Errorf("failed to save migrated epoch %d data: %w", epochID, err)
			}
			bk.Logger().Info("Successfully precomputed slot public keys for epoch", "epoch_id", epochID)
		}
	}

	return nil
}

func setV0_2_8Params(ctx context.Context, k keeper.Keeper) error {
	params, err := k.GetParams(ctx)
	if err != nil {
		return err
	}

	if params.PocParams != nil {
		params.PocParams.ModelId = "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"
		params.PocParams.SeqLen = 1024
		params.PocParams.PocV2Enabled = false
		params.PocParams.ConfirmationPocV2Enabled = true
		params.PocParams.StatTest = &types.PoCStatTestParams{
			DistThreshold:   types.DecimalFromFloat(0.4),
			PMismatch:       types.DecimalFromFloat(0.1),
			PValueThreshold: types.DecimalFromFloat(0.05),
		}
	}

	return k.SetParams(ctx, params)
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
