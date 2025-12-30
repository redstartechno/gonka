package keeper

import (
	"fmt"

	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

type PocWindowType int

const (
	PocWindowBatch PocWindowType = iota
	PocWindowValidation
)

func (k Keeper) ValidatePocPeriod(ctx sdk.Context, startBlockHeight int64, windowType PocWindowType) error {
	currentBlockHeight := ctx.BlockHeight()

	activeEvent, isActive, err := k.GetActiveConfirmationPoCEvent(ctx)
	if err != nil {
		k.Logger().Error("[ValidatePocPeriod] Error checking confirmation PoC event", "error", err)
	}

	if isActive && activeEvent != nil {
		return k.validateConfirmationPoc(ctx, activeEvent, startBlockHeight, currentBlockHeight, windowType)
	}

	return k.validateRegularPoc(ctx, startBlockHeight, currentBlockHeight, windowType)
}

func (k Keeper) validateConfirmationPoc(ctx sdk.Context, event *types.ConfirmationPoCEvent, startBlockHeight, currentBlockHeight int64, windowType PocWindowType) error {
	if startBlockHeight != event.TriggerHeight {
		k.Logger().Error(
			"[ValidatePocPeriod] Confirmation PoC: start block height mismatch",
			"startBlockHeight", startBlockHeight,
			"triggerHeight", event.TriggerHeight,
			"currentBlockHeight", currentBlockHeight,
		)
		return errorsmod.Wrapf(
			types.ErrPocWrongStartBlockHeight,
			"Confirmation PoC start block height mismatch: expected %d, got %d",
			event.TriggerHeight, startBlockHeight,
		)
	}

	epochParams := k.GetParams(ctx).EpochParams

	switch windowType {
	case PocWindowBatch:
		if event.Phase != types.ConfirmationPoCPhase_CONFIRMATION_POC_GENERATION {
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC not in generation phase")
		}
		if !event.IsInBatchSubmissionWindow(currentBlockHeight, epochParams) {
			k.Logger().Error(
				"[ValidatePocPeriod] Confirmation PoC: outside batch submission window",
				"currentBlockHeight", currentBlockHeight,
				"generationStartHeight", event.GenerationStartHeight,
				"exchangeEndHeight", event.GetExchangeEnd(epochParams),
			)
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC batch submission window closed")
		}

	case PocWindowValidation:
		if event.Phase != types.ConfirmationPoCPhase_CONFIRMATION_POC_VALIDATION {
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC not in validation phase")
		}
		if !event.IsInValidationWindow(currentBlockHeight, epochParams) {
			k.Logger().Error(
				"[ValidatePocPeriod] Confirmation PoC: outside validation window",
				"currentBlockHeight", currentBlockHeight,
				"validationStartHeight", event.GetValidationStart(epochParams),
				"validationEndHeight", event.GetValidationEnd(epochParams),
			)
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC validation window closed")
		}
	}

	return nil
}

func (k Keeper) validateRegularPoc(ctx sdk.Context, startBlockHeight, currentBlockHeight int64, windowType PocWindowType) error {
	epochParams := k.GetParams(ctx).EpochParams
	upcomingEpoch, found := k.GetUpcomingEpoch(ctx)
	if !found {
		k.Logger().Error(
			"[ValidatePocPeriod] Failed to get upcoming epoch",
			"currentBlockHeight", currentBlockHeight,
		)
		return errorsmod.Wrap(
			types.ErrUpcomingEpochNotFound,
			fmt.Sprintf("Failed to get upcoming epoch at block %d", currentBlockHeight),
		)
	}

	epochContext := types.NewEpochContext(*upcomingEpoch, *epochParams)

	if !epochContext.IsStartOfPocStage(startBlockHeight) {
		k.Logger().Error(
			"[ValidatePocPeriod] Start block height doesn't match epoch",
			"startBlockHeight", startBlockHeight,
			"expectedStartBlockHeight", epochContext.PocStartBlockHeight,
			"currentBlockHeight", currentBlockHeight,
		)
		return errorsmod.Wrapf(
			types.ErrPocWrongStartBlockHeight,
			"Start block height %d doesn't match epoch PoC start %d",
			startBlockHeight, epochContext.PocStartBlockHeight,
		)
	}

	switch windowType {
	case PocWindowBatch:
		if !epochContext.IsPoCExchangeWindow(currentBlockHeight) {
			k.Logger().Error(
				"[ValidatePocPeriod] PoC exchange window closed",
				"startBlockHeight", startBlockHeight,
				"currentBlockHeight", currentBlockHeight,
				"pocStartBlockHeight", epochContext.PocStartBlockHeight,
			)
			return errorsmod.Wrapf(
				types.ErrPocTooLate,
				"PoC exchange window closed at block %d",
				currentBlockHeight,
			)
		}

	case PocWindowValidation:
		if !epochContext.IsValidationExchangeWindow(currentBlockHeight) {
			k.Logger().Error(
				"[ValidatePocPeriod] Validation exchange window closed",
				"startBlockHeight", startBlockHeight,
				"currentBlockHeight", currentBlockHeight,
				"pocStartBlockHeight", epochContext.PocStartBlockHeight,
			)
			return errorsmod.Wrapf(
				types.ErrPocTooLate,
				"Validation exchange window closed at block %d",
				currentBlockHeight,
			)
		}
	}

	return nil
}
