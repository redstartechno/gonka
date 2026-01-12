package keeper

import (
	"context"

	sdkerrors "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

// SubmitPocArtifactBatchesV2 handles submission of PoC v2 artifact batches from multiple nodes.
// Note: Current iteration stores on-chain; later iteration moves fully off-chain.
func (k msgServer) SubmitPocArtifactBatchesV2(goCtx context.Context, msg *types.MsgSubmitPocArtifactBatchesV2) (*types.MsgSubmitPocArtifactBatchesV2Response, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Participant access gating: blocklisted accounts cannot participate in PoC.
	if k.IsPoCParticipantBlocked(ctx, msg.Creator) {
		k.LogError(PocFailureTag+"[SubmitPocArtifactBatchesV2] participant is blocked from PoC", types.PoC, "participant", msg.Creator)
		return nil, sdkerrors.Wrap(types.ErrParticipantBlocked, msg.Creator)
	}

	currentBlockHeight := ctx.BlockHeight()

	for i, batch := range msg.Batches {
		if batch.NodeId == "" {
			k.LogError(PocFailureTag+"[SubmitPocArtifactBatchesV2] NodeId is empty", types.PoC,
				"participant", msg.Creator,
				"batchIndex", i)
			return nil, sdkerrors.Wrap(types.ErrPocNodeIdEmpty, "NodeId is empty")
		}

		startBlockHeight := batch.PocStageStartBlockHeight

		// Reuse existing PoC window semantics
		epochParams := k.Keeper.GetParams(goCtx).EpochParams
		upcomingEpoch, found := k.Keeper.GetUpcomingEpoch(ctx)
		if !found {
			k.LogError(PocFailureTag+"[SubmitPocArtifactBatchesV2] Failed to get upcoming epoch", types.PoC,
				"participant", msg.Creator,
				"batchIndex", i)
			return nil, sdkerrors.Wrap(types.ErrUpcomingEpochNotFound, "Failed to get upcoming epoch")
		}
		epochContext := types.NewEpochContext(*upcomingEpoch, *epochParams)

		if !epochContext.IsStartOfPocStage(startBlockHeight) {
			k.LogError(PocFailureTag+"[SubmitPocArtifactBatchesV2] start block height mismatch", types.PoC,
				"participant", msg.Creator,
				"batchIndex", i,
				"batch.PocStageStartBlockHeight", startBlockHeight,
				"epochContext.PocStartBlockHeight", epochContext.PocStartBlockHeight)
			return nil, sdkerrors.Wrap(types.ErrPocWrongStartBlockHeight, "start block height mismatch")
		}

		if !epochContext.IsPoCExchangeWindow(currentBlockHeight) {
			k.LogError(PocFailureTag+"[SubmitPocArtifactBatchesV2] PoC exchange window is closed", types.PoC,
				"participant", msg.Creator,
				"batchIndex", i,
				"currentBlockHeight", currentBlockHeight)
			return nil, sdkerrors.Wrap(types.ErrPocTooLate, "PoC exchange window is closed")
		}

		// Store the v2 batch with creator as participant
		storedBatch := types.PoCArtifactBatchV2{
			ParticipantAddress:       msg.Creator,
			PocStageStartBlockHeight: startBlockHeight,
			NodeId:                   batch.NodeId,
			Artifacts:                batch.Artifacts,
		}

		k.SetPocArtifactBatchV2(ctx, storedBatch)

		k.LogInfo("[SubmitPocArtifactBatchesV2] Artifact batch stored", types.PoC,
			"participant", msg.Creator,
			"startBlockHeight", startBlockHeight,
			"nodeId", batch.NodeId,
			"artifactsCount", len(batch.Artifacts))
	}

	return &types.MsgSubmitPocArtifactBatchesV2Response{}, nil
}

// SubmitPocValidationsV2 handles batch submission of PoC v2 validations.
func (k msgServer) SubmitPocValidationsV2(goCtx context.Context, msg *types.MsgSubmitPocValidationsV2) (*types.MsgSubmitPocValidationsV2Response, error) {
	ctx := sdk.UnwrapSDKContext(goCtx)

	// Participant access gating: blocklisted accounts cannot validate in PoC.
	if k.IsPoCParticipantBlocked(ctx, msg.Creator) {
		k.LogError(PocFailureTag+"[SubmitPocValidationsV2] validator is blocked from PoC", types.PoC, "validator", msg.Creator)
		return nil, sdkerrors.Wrap(types.ErrParticipantBlocked, msg.Creator)
	}

	currentBlockHeight := ctx.BlockHeight()

	for i, validation := range msg.Validations {
		startBlockHeight := validation.PocStageStartBlockHeight

		// Reuse existing PoC window semantics for each validation
		epochParams := k.Keeper.GetParams(goCtx).EpochParams
		upcomingEpoch, found := k.Keeper.GetUpcomingEpoch(ctx)
		if !found {
			k.LogError(PocFailureTag+"[SubmitPocValidationsV2] Failed to get upcoming epoch", types.PoC,
				"validator", msg.Creator,
				"validationIndex", i)
			return nil, sdkerrors.Wrap(types.ErrUpcomingEpochNotFound, "Failed to get upcoming epoch")
		}
		epochContext := types.NewEpochContext(*upcomingEpoch, *epochParams)

		if !epochContext.IsStartOfPocStage(startBlockHeight) {
			k.LogError(PocFailureTag+"[SubmitPocValidationsV2] start block height mismatch", types.PoC,
				"validator", msg.Creator,
				"participant", validation.ParticipantAddress,
				"validationIndex", i)
			return nil, sdkerrors.Wrap(types.ErrPocWrongStartBlockHeight, "start block height mismatch")
		}

		if !epochContext.IsValidationExchangeWindow(currentBlockHeight) {
			k.LogError(PocFailureTag+"[SubmitPocValidationsV2] PoC validation exchange window is closed", types.PoC,
				"validator", msg.Creator,
				"participant", validation.ParticipantAddress,
				"validationIndex", i)
			return nil, sdkerrors.Wrap(types.ErrPocTooLate, "PoC validation exchange window is closed")
		}

		// Store the v2 validation
		storedValidation := types.PoCValidationV2{
			ParticipantAddress:          validation.ParticipantAddress,
			ValidatorParticipantAddress: msg.Creator,
			PocStageStartBlockHeight:    startBlockHeight,
			ValidatedWeight:             validation.ValidatedWeight,
		}

		k.SetPocValidationV2(ctx, storedValidation)

		k.LogInfo("[SubmitPocValidationsV2] Validation stored", types.PoC,
			"validator", msg.Creator,
			"participant", validation.ParticipantAddress,
			"validatedWeight", validation.ValidatedWeight)
	}

	return &types.MsgSubmitPocValidationsV2Response{}, nil
}
