package v0_2_13

import (
	"cmp"
	"context"
	"slices"

	upgradetypes "cosmossdk.io/x/upgrade/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

const (
	MaxEscrowsPerEpoch uint32 = 500_000
	MaxNonce           uint32 = 1_000_000
)

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

		if err := setDevshardEscrowParams(ctx, k); err != nil {
			return nil, err
		}
		if err := backfillConfirmationWeightScales(ctx, k); err != nil {
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

func setDevshardEscrowParams(ctx context.Context, k keeper.Keeper) error {
	params, err := k.GetParams(ctx)
	if err != nil {
		return err
	}
	if params.DevshardEscrowParams == nil {
		params.DevshardEscrowParams = types.DefaultDevshardEscrowParams()
	}
	params.DevshardEscrowParams.MaxEscrowsPerEpoch = MaxEscrowsPerEpoch
	params.DevshardEscrowParams.MaxNonce = MaxNonce
	if err := k.SetParams(ctx, params); err != nil {
		return err
	}
	k.LogInfo("set devshard escrow params", types.Upgrades,
		"max_escrows_per_epoch", MaxEscrowsPerEpoch,
		"max_nonce", MaxNonce)
	return nil
}

func backfillConfirmationWeightScales(ctx context.Context, k keeper.Keeper) error {
	epochIndex, found := k.GetEffectiveEpochIndex(ctx)
	if !found {
		k.LogWarn("confirmation weight scales backfill skipped: no effective epoch", types.Upgrades)
		return nil
	}
	root, found := k.GetEpochGroupData(ctx, epochIndex, "")
	if !found {
		k.LogWarn("confirmation weight scales backfill skipped: root epoch group missing", types.Upgrades,
			"epoch", epochIndex)
		return nil
	}
	activeParticipants, found := k.GetActiveParticipants(ctx, epochIndex)
	if !found {
		k.LogWarn("confirmation weight scales backfill skipped: active participants missing", types.Upgrades,
			"epoch", epochIndex)
		return nil
	}
	params, err := k.GetParams(ctx)
	if err != nil {
		return err
	}

	confirmableModels := make(map[string]bool)
	for _, groupData := range k.GetAllEpochGroupData(ctx) {
		if groupData.EpochIndex != epochIndex || groupData.ModelId == "" {
			continue
		}
		for _, vw := range groupData.ValidationWeights {
			if vw != nil && vw.VotingPower > 0 {
				confirmableModels[groupData.ModelId] = true
				break
			}
		}
	}

	root.ConfirmationWeightScales = confirmationWeightScalesFromModels(confirmableModels, params.PocParams)
	activeByAddress := make(map[string]*types.ActiveParticipant, len(activeParticipants.Participants))
	for _, p := range activeParticipants.Participants {
		if p != nil {
			activeByAddress[p.Index] = p
		}
	}
	for _, vw := range root.ValidationWeights {
		if vw == nil {
			continue
		}
		p := activeByAddress[vw.MemberAddress]
		if p == nil {
			continue
		}
		expected := types.ConfirmationWeightOfParticipant(p, root.ConfirmationWeightScales)
		if vw.ConfirmationWeight > expected {
			vw.ConfirmationWeight = expected
		}
	}
	k.SetEpochGroupData(ctx, root)
	k.LogInfo("backfilled confirmation weight scales", types.Upgrades,
		"epoch", epochIndex,
		"models", len(root.ConfirmationWeightScales))
	return nil
}

func confirmationWeightScalesFromModels(
	models map[string]bool,
	pocParams *types.PocParams,
) []*types.ConfirmationWeightScale {
	coefficients := make(map[string]*types.Decimal)
	for _, config := range pocParams.GetModelConfigs() {
		if config == nil || config.ModelId == "" {
			continue
		}
		coefficients[config.ModelId] = config.WeightScaleFactor
	}

	modelIDs := make([]string, 0, len(models))
	for modelID := range models {
		modelIDs = append(modelIDs, modelID)
	}
	slices.SortFunc(modelIDs, cmp.Compare)

	scales := make([]*types.ConfirmationWeightScale, 0, len(modelIDs))
	for _, modelID := range modelIDs {
		scales = append(scales, &types.ConfirmationWeightScale{
			ModelId:           modelID,
			WeightScaleFactor: coefficients[modelID].CloneOrOne(),
		})
	}
	return scales
}
