package keeper

import (
	"context"

	sdkerrors "cosmossdk.io/errors"
	"github.com/productscience/inference/x/inference/types"
)

const PocFailureTag = "[PoC Failure]"

func (k msgServer) SubmitPocValidation(goCtx context.Context, msg *types.MsgSubmitPocValidation) (*types.MsgSubmitPocValidationResponse, error) {
	return nil, sdkerrors.Wrap(types.ErrDeprecated, "MsgSubmitPocValidation is deprecated, use MsgSubmitPocValidationsV2 instead")
}
