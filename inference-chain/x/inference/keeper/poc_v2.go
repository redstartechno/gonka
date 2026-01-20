package keeper

import (
	"context"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

// SetPocValidationV2 stores a PoC v2 validation.
func (k Keeper) SetPocValidationV2(ctx context.Context, validation types.PoCValidationV2) {
	participantAddr := sdk.MustAccAddressFromBech32(validation.ParticipantAddress)
	validatorAddr := sdk.MustAccAddressFromBech32(validation.ValidatorParticipantAddress)
	pk := collections.Join3(validation.PocStageStartBlockHeight, participantAddr, validatorAddr)
	k.LogInfo("PoC v2: Storing validation", types.PoC,
		"epoch", validation.PocStageStartBlockHeight,
		"participant", validation.ParticipantAddress,
		"validator", validation.ValidatorParticipantAddress,
		"validated_weight", validation.ValidatedWeight)
	if err := k.PoCValidationsV2.Set(ctx, pk, validation); err != nil {
		panic(err)
	}
}

// GetPoCValidationsV2ByStage collects all PoCValidationV2 grouped by participant for a specific epoch.
func (k Keeper) GetPoCValidationsV2ByStage(ctx context.Context, pocStageStartBlockHeight int64) (map[string][]types.PoCValidationV2, error) {
	result := make(map[string][]types.PoCValidationV2)

	iter, err := k.PoCValidationsV2.Iterate(ctx, collections.NewPrefixedTripleRange[int64, sdk.AccAddress, sdk.AccAddress](pocStageStartBlockHeight))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		validation, err := iter.Value()
		if err != nil {
			return nil, err
		}
		result[validation.ParticipantAddress] = append(result[validation.ParticipantAddress], validation)
	}

	return result, nil
}

// GetAllPoCV2StoreCommitsForStage returns all store commits for a given PoC stage, keyed by participant address.
func (k Keeper) GetAllPoCV2StoreCommitsForStage(ctx context.Context, pocStageStartBlockHeight int64) (map[string]types.PoCV2StoreCommit, error) {
	result := make(map[string]types.PoCV2StoreCommit)

	iter, err := k.PoCV2StoreCommits.Iterate(ctx, collections.NewPrefixedPairRange[int64, sdk.AccAddress](pocStageStartBlockHeight))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return nil, err
		}
		value, err := iter.Value()
		if err != nil {
			return nil, err
		}
		addr := key.K2()
		result[addr.String()] = value
	}

	return result, nil
}

// GetAllMLNodeWeightDistributionsForStage returns all weight distributions for a given PoC stage, keyed by participant address.
func (k Keeper) GetAllMLNodeWeightDistributionsForStage(ctx context.Context, pocStageStartBlockHeight int64) (map[string]types.MLNodeWeightDistribution, error) {
	result := make(map[string]types.MLNodeWeightDistribution)

	iter, err := k.MLNodeWeightDistributions.Iterate(ctx, collections.NewPrefixedPairRange[int64, sdk.AccAddress](pocStageStartBlockHeight))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return nil, err
		}
		value, err := iter.Value()
		if err != nil {
			return nil, err
		}
		addr := key.K2()
		result[addr.String()] = value
	}

	return result, nil
}

// SetPoCV2StoreCommit stores a PoCV2StoreCommit (for testing).
func (k Keeper) SetPoCV2StoreCommit(ctx context.Context, commit types.PoCV2StoreCommit) {
	addr := sdk.MustAccAddressFromBech32(commit.ParticipantAddress)
	pk := collections.Join(commit.PocStageStartBlockHeight, addr)
	if err := k.PoCV2StoreCommits.Set(ctx, pk, commit); err != nil {
		panic(err)
	}
}

// SetMLNodeWeightDistribution stores an MLNodeWeightDistribution (for testing).
func (k Keeper) SetMLNodeWeightDistribution(ctx context.Context, distribution types.MLNodeWeightDistribution) {
	addr := sdk.MustAccAddressFromBech32(distribution.ParticipantAddress)
	pk := collections.Join(distribution.PocStageStartBlockHeight, addr)
	if err := k.MLNodeWeightDistributions.Set(ctx, pk, distribution); err != nil {
		panic(err)
	}
}
