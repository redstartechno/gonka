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

func (k Keeper) CheckPocMessageTooLate(ctx sdk.Context, startBlockHeight int64, windowType PocWindowType) error {
	currentBlockHeight := ctx.BlockHeight()

	if startBlockHeight >= currentBlockHeight {
		return nil
	}

	activeEvent, isActive, err := k.GetActiveConfirmationPoCEvent(ctx)
	if err != nil {
		k.Logger().Error("[ValidatePocPeriod] Error checking confirmation PoC event", "error", err)
	}

	if isActive && activeEvent != nil {
		return k.checkConfirmationPocMessageTooLate(ctx, activeEvent, startBlockHeight, currentBlockHeight, windowType)
	}

	return k.checkRegularPocMessageTooLate(ctx, startBlockHeight, currentBlockHeight, windowType)
}

func (k Keeper) checkConfirmationPocMessageTooLate(ctx sdk.Context, event *types.ConfirmationPoCEvent, startBlockHeight, currentBlockHeight int64, windowType PocWindowType) error {
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
		if event.Phase > types.ConfirmationPoCPhase_CONFIRMATION_POC_GENERATION {
			k.Logger().Error(
				"[ValidatePocPeriod] Confirmation PoC: outside batch submission window",
				"currentBlockHeight", currentBlockHeight,
				"generationStartHeight", event.GenerationStartHeight,
				"exchangeEndHeight", event.GetExchangeEnd(epochParams),
			)
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC is past generation phase")
		}

	case PocWindowValidation:
		if event.Phase > types.ConfirmationPoCPhase_CONFIRMATION_POC_VALIDATION {
			k.Logger().Error(
				"[ValidatePocPeriod] Confirmation PoC: outside validation window",
				"currentBlockHeight", currentBlockHeight,
				"validationStartHeight", event.GetValidationStart(epochParams),
				"validationEndHeight", event.GetValidationEnd(epochParams),
			)
			return errorsmod.Wrap(types.ErrPocTooLate, "Confirmation PoC not in validation phase")
		}
	}

	return nil
}

func (k Keeper) checkRegularPocMessageTooLate(ctx sdk.Context, startBlockHeight, currentBlockHeight int64, windowType PocWindowType) error {
	epochParams := k.GetParams(ctx).EpochParams
	currentEpoch, found := k.GetEffectiveEpoch(ctx)
	if !found {
		k.Logger().Error(
			"[ValidatePocPeriod] Failed to get effective epoch",
			"currentBlockHeight", currentBlockHeight,
		)
		return nil
	}
	currentEpochContext := types.NewEpochContext(*currentEpoch, *epochParams)
	if startBlockHeight <= currentEpochContext.StartOfPoC() {
		k.Logger().Error(
			"[ValidatePocPeriod] Start block height is for PoC stage that already finished",
			"currentBlockHeight", currentBlockHeight,
			"startBlockHeight", startBlockHeight,
			"startOfPoC", currentEpochContext.StartOfPoC(),
		)
		return errorsmod.Wrap(
			types.ErrUpcomingEpochNotFound,
			fmt.Sprintf("PoC stage already finished %d", currentBlockHeight),
		)
	}
	// startBlockHeight > currentEpochContext.StartOfPoC()

	upcomingEpoch, found := k.GetUpcomingEpoch(ctx)
	if !found {
		k.Logger().Error(
			"[ValidatePocPeriod] Failed to get upcoming epoch while current block is past startBlock",
			"currentBlockHeight", currentBlockHeight,
			"startBlockHeight", startBlockHeight,
			"startOfPoC", currentEpochContext.StartOfPoC(),
		)
		return errorsmod.Wrap(
			types.ErrUpcomingEpochNotFound,
			fmt.Sprintf("PoC stage already finished %d", currentBlockHeight),
		)
	}

	upcomingEpochContext := types.NewEpochContext(*upcomingEpoch, *epochParams)

	if !upcomingEpochContext.IsStartOfPocStage(startBlockHeight) {
		k.Logger().Error(
			"[ValidatePocPeriod] Start block height doesn't match upcoming epoch",
			"startBlockHeight", startBlockHeight,
			"expectedStartBlockHeight", upcomingEpochContext.PocStartBlockHeight,
			"currentBlockHeight", currentBlockHeight,
		)
		return errorsmod.Wrapf(
			types.ErrPocWrongStartBlockHeight,
			"Start block height %d doesn't match upcoming epoch PoC start %d",
			startBlockHeight, upcomingEpochContext.PocStartBlockHeight,
		)
	}

	switch windowType {
	case PocWindowBatch:
		if currentBlockHeight > upcomingEpochContext.PoCExchangeDeadline() {
			k.Logger().Error(
				"[ValidatePocPeriod] PoC exchange window closed",
				"startBlockHeight", startBlockHeight,
				"currentBlockHeight", currentBlockHeight,
				"pocStartBlockHeight", upcomingEpochContext.PocStartBlockHeight,
				"pocExchangeDeadline", upcomingEpochContext.PoCExchangeDeadline(),
			)
			return errorsmod.Wrapf(
				types.ErrPocTooLate,
				"PoC exchange window closed at block %d",
				currentBlockHeight,
			)
		}

	case PocWindowValidation:
		if currentBlockHeight > upcomingEpochContext.EndOfPoCValidation() {
			k.Logger().Error(
				"[ValidatePocPeriod] Validation exchange window closed",
				"startBlockHeight", startBlockHeight,
				"currentBlockHeight", currentBlockHeight,
				"pocStartBlockHeight", upcomingEpochContext.PocStartBlockHeight,
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
