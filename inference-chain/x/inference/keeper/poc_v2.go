package keeper

import (
	"context"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

// SetPocBatchV2 stores a PoC v2 batch.
func (k Keeper) SetPocBatchV2(ctx context.Context, batch types.PoCBatchV2) {
	addr := sdk.MustAccAddressFromBech32(batch.ParticipantAddress)
	// Use node_id as the third key component (similar to batch_id in v1)
	pk := collections.Join3(batch.PocStageStartBlockHeight, addr, batch.NodeId)
	k.LogInfo("PoC v2: Storing batch", types.PoC,
		"epoch", batch.PocStageStartBlockHeight,
		"participant", batch.ParticipantAddress,
		"node_id", batch.NodeId,
		"artifacts_count", len(batch.Artifacts))
	if err := k.PoCBatchesV2.Set(ctx, pk, batch); err != nil {
		panic(err)
	}
}

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

// GetPoCBatchesV2ByStage collects all PoCBatchV2 grouped by participant for a specific epoch.
func (k Keeper) GetPoCBatchesV2ByStage(ctx context.Context, pocStageStartBlockHeight int64) (map[string][]types.PoCBatchV2, error) {
	result := make(map[string][]types.PoCBatchV2)

	iter, err := k.PoCBatchesV2.Iterate(ctx, collections.NewPrefixedTripleRange[int64, sdk.AccAddress, string](pocStageStartBlockHeight))
	if err != nil {
		return nil, err
	}
	defer iter.Close()

	for ; iter.Valid(); iter.Next() {
		batch, err := iter.Value()
		if err != nil {
			return nil, err
		}
		result[batch.ParticipantAddress] = append(result[batch.ParticipantAddress], batch)
	}

	return result, nil
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
