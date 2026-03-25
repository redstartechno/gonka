package keeper_test

import (
	"testing"

	testkeeper "github.com/productscience/inference/testutil/keeper"
	"github.com/productscience/inference/x/inference/types"
	"github.com/stretchr/testify/require"
)

func TestFeeParams_DefaultsWhenNotSet(t *testing.T) {
	k, ctx := testkeeper.InferenceKeeper(t)

	fp := k.GetFeeParams(ctx)
	require.Equal(t, types.DefaultFeeParams(), fp)
}

func TestFeeParams_SetAndGet(t *testing.T) {
	k, ctx := testkeeper.InferenceKeeper(t)

	custom := types.FeeParams{
		MinGasPriceNgonka: 42,
		BaseValidationGas: 1_000_000,
		GasPerPoCCount:    200,
	}
	require.NoError(t, k.SetFeeParams(ctx, custom))

	fp := k.GetFeeParams(ctx)
	require.Equal(t, custom, fp)
}

func TestFeeParams_ZeroValues(t *testing.T) {
	k, ctx := testkeeper.InferenceKeeper(t)

	zero := types.FeeParams{
		MinGasPriceNgonka: 0,
		BaseValidationGas: 0,
		GasPerPoCCount:    0,
	}
	require.NoError(t, k.SetFeeParams(ctx, zero))

	fp := k.GetFeeParams(ctx)
	require.Equal(t, zero, fp)
}
