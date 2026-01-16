package keeper

import (
	"context"

	sdkerrors "cosmossdk.io/errors"
	"github.com/productscience/inference/x/inference/types"
)

func (k msgServer) SubmitPocBatch(goCtx context.Context, msg *types.MsgSubmitPocBatch) (*types.MsgSubmitPocBatchResponse, error) {
	return nil, sdkerrors.Wrap(types.ErrDeprecated, "MsgSubmitPocBatch is deprecated, use MsgSubmitPocBatchesV2 instead")
}
