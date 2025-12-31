package app

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	authztypes "github.com/cosmos/cosmos-sdk/x/authz"
	inferencemodulekeeper "github.com/productscience/inference/x/inference/keeper"
	inferencetypes "github.com/productscience/inference/x/inference/types"
)

type PocPeriodValidationDecorator struct {
	inferenceKeeper *inferencemodulekeeper.Keeper
}

func NewPocPeriodValidationDecorator(ik *inferencemodulekeeper.Keeper) PocPeriodValidationDecorator {
	return PocPeriodValidationDecorator{
		inferenceKeeper: ik,
	}
}

// validatePocMessage validates a single PoC message (either direct or nested)
func (ppd PocPeriodValidationDecorator) checkPocMessageTooLate(ctx sdk.Context, msg sdk.Msg) error {
	switch m := msg.(type) {
	case *inferencetypes.MsgSubmitPocBatch:
		if err := ppd.inferenceKeeper.CheckPocMessageTooLate(ctx, m.PocStageStartBlockHeight, inferencemodulekeeper.PocWindowBatch); err != nil {
			return err
		}
	case *inferencetypes.MsgSubmitPocValidation:
		if err := ppd.inferenceKeeper.CheckPocMessageTooLate(ctx, m.PocStageStartBlockHeight, inferencemodulekeeper.PocWindowValidation); err != nil {
			return err
		}
	case *authztypes.MsgExec:
		// Recursively validate messages inside MsgExec
		for _, innerMsg := range m.Msgs {
			var unwrapped sdk.Msg
			if err := ppd.inferenceKeeper.Codec().UnpackAny(innerMsg, &unwrapped); err != nil {
				// If we can't unpack, skip (but log in production)
				continue
			}
			if err := ppd.checkPocMessageTooLate(ctx, unwrapped); err != nil {
				return err
			}
		}
	}
	return nil
}

func (ppd PocPeriodValidationDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (sdk.Context, error) {
	if simulate {
		return next(ctx, tx, simulate)
	}

	// Only perform validation during CheckTx (including ReCheckTx)
	if !ctx.IsCheckTx() {
		return next(ctx, tx, simulate)
	}

	for _, msg := range tx.GetMsgs() {
		if err := ppd.checkPocMessageTooLate(ctx, msg); err != nil {
			return ctx, err
		}
	}

	return next(ctx, tx, simulate)
}
