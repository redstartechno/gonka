package keeper

import (
	"context"

	"github.com/cosmos/cosmos-sdk/runtime"
	"github.com/productscience/inference/x/inference/types"
)

// GetFeeParams returns the fee parameters from the KV store.
// Returns defaults if not yet set.
func (k Keeper) GetFeeParams(ctx context.Context) types.FeeParams {
	store := runtime.KVStoreAdapter(k.storeService.OpenKVStore(ctx))
	bz := store.Get(types.FeeParamsKey)
	if bz == nil {
		return types.DefaultFeeParams()
	}
	fp, err := types.UnmarshalFeeParams(bz)
	if err != nil {
		k.LogError("Unable to unmarshal FeeParams, using defaults", types.System, "error", err)
		return types.DefaultFeeParams()
	}
	return fp
}

// SetFeeParams stores the fee parameters in the KV store.
func (k Keeper) SetFeeParams(ctx context.Context, fp types.FeeParams) error {
	store := runtime.KVStoreAdapter(k.storeService.OpenKVStore(ctx))
	bz, err := fp.Marshal()
	if err != nil {
		return err
	}
	store.Set(types.FeeParamsKey, bz)
	return nil
}
