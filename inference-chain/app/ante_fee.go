package app

import (
	"fmt"

	"cosmossdk.io/math"
	errorsmod "cosmossdk.io/errors"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	"github.com/cosmos/cosmos-sdk/x/auth/ante"
	authztypes "github.com/cosmos/cosmos-sdk/x/authz"

	inferencemodulekeeper "github.com/productscience/inference/x/inference/keeper"
	inferencetypes "github.com/productscience/inference/x/inference/types"

	blstypes "github.com/productscience/inference/x/bls/types"
)

// --- Context key for fee bypass flag ---

type networkDutyFeeBypassKey struct{}

// IsNetworkDutyBypassed returns true if the NetworkDutyFeeBypassDecorator has
// determined that all messages in the transaction are fee-exempt network duties.
func IsNetworkDutyBypassed(ctx sdk.Context) bool {
	v, ok := ctx.Value(networkDutyFeeBypassKey{}).(bool)
	return ok && v
}

// --- NetworkDutyFeeBypassDecorator ---

// NetworkDutyFeeBypassDecorator exempts transactions containing only protocol-duty
// messages from fee requirements. It clears min gas prices and sets a context flag
// that the custom TxFeeChecker respects.
//
// Placed before DeductFeeDecorator in the ante chain.
// Follows the same pattern as LiquidityPoolFeeBypassDecorator.
type NetworkDutyFeeBypassDecorator struct {
	InferenceKeeper *inferencemodulekeeper.Keeper
	GasCap          uint64 // maximum gas for bypassed txs to prevent block-space abuse
	Priority        int64  // priority boost so zero-fee duty txs aren't starved
}

func (d NetworkDutyFeeBypassDecorator) AnteHandle(ctx sdk.Context, tx sdk.Tx, simulate bool, next sdk.AnteHandler) (sdk.Context, error) {
	msgs := tx.GetMsgs()
	if len(msgs) == 0 {
		return next(ctx, tx, simulate)
	}

	// Check if ALL messages are fee-exempt network duties.
	allExempt := true
	for _, msg := range msgs {
		if !isNetworkDutyRecursive(msg, d.InferenceKeeper) {
			allExempt = false
			break
		}
	}

	if !allExempt {
		return next(ctx, tx, simulate)
	}

	// Enforce gas cap on bypassed transactions.
	if feeTx, ok := tx.(sdk.FeeTx); ok {
		if d.GasCap > 0 && feeTx.GetGas() > d.GasCap {
			return ctx, fmt.Errorf("fee-bypass: gas %d exceeds cap %d for network-duty tx", feeTx.GetGas(), d.GasCap)
		}
	}

	if d.InferenceKeeper != nil {
		d.InferenceKeeper.LogDebug("AnteHandle: NetworkDutyFeeBypass - applying fee bypass",
			inferencetypes.System)
	}

	// Clear min gas prices and set bypass flag for the custom TxFeeChecker.
	ctx = ctx.WithMinGasPrices(sdk.DecCoins{})
	ctx = ctx.WithValue(networkDutyFeeBypassKey{}, true)
	if d.Priority != 0 {
		ctx = ctx.WithPriority(d.Priority)
	}

	return next(ctx, tx, simulate)
}

// isNetworkDutyRecursive checks if a message is a fee-exempt network duty,
// recursively unpacking x/authz MsgExec wrappers. Fails closed: if any inner
// message is not exempt, returns false.
func isNetworkDutyRecursive(msg sdk.Msg, ik *inferencemodulekeeper.Keeper) bool {
	if execMsg, ok := msg.(*authztypes.MsgExec); ok {
		if ik == nil {
			return false // fail closed
		}
		for _, innerMsg := range execMsg.Msgs {
			var unwrapped sdk.Msg
			if err := ik.Codec().UnpackAny(innerMsg, &unwrapped); err != nil {
				return false // fail closed on unpack error
			}
			if !isNetworkDutyRecursive(unwrapped, ik) {
				return false
			}
		}
		return true
	}
	return isExemptMessageType(msg)
}

// isExemptMessageType returns true for messages that are protocol obligations.
// These are already rate-limited by timing windows, duplicate checks, or allowlists.
func isExemptMessageType(msg sdk.Msg) bool {
	switch msg.(type) {
	// PoC duty messages (throttled by PocPeriodValidationDecorator window checks)
	case *inferencetypes.MsgSubmitPocBatch,
		*inferencetypes.MsgSubmitPocValidation,
		*inferencetypes.MsgSubmitPocValidationsV2,
		*inferencetypes.MsgMLNodeWeightDistribution:
		return true

	// Inference validation duty (throttled by ValidationEarlyRejectDecorator)
	case *inferencetypes.MsgValidation:
		return true

	// Inference lifecycle (TA-whitelisted for start, host-submitted for finish)
	case *inferencetypes.MsgStartInference,
		*inferencetypes.MsgFinishInference:
		return true

	// Inference challenges (hosts are required to submit these)
	case *inferencetypes.MsgInvalidateInference,
		*inferencetypes.MsgRevalidateInference:
		return true

	// BLS DKG protocol messages (epoch-scoped)
	case *blstypes.MsgSubmitDealerPart,
		*blstypes.MsgSubmitVerificationVector,
		*blstypes.MsgSubmitGroupKeyValidationSignature,
		*blstypes.MsgSubmitPartialSignature,
		*blstypes.MsgRequestThresholdSignature:
		return true

	default:
		return false
	}
}

// --- Custom TxFeeChecker ---

// GonkaFeeChecker returns a TxFeeChecker that enforces a consensus-level minimum
// gas price read from on-chain FeeParams. It respects the bypass flag set by
// NetworkDutyFeeBypassDecorator. This checker runs inside DeductFeeDecorator
// during both CheckTx and DeliverTx.
func GonkaFeeChecker(inferenceKeeper *inferencemodulekeeper.Keeper) ante.TxFeeChecker {
	return func(ctx sdk.Context, tx sdk.Tx) (sdk.Coins, int64, error) {
		// If bypass flag is set, allow zero fees.
		if IsNetworkDutyBypassed(ctx) {
			return sdk.Coins{}, 0, nil
		}

		feeTx, ok := tx.(sdk.FeeTx)
		if !ok {
			return nil, 0, errorsmod.Wrap(sdkerrors.ErrTxDecode, "Tx must implement FeeTx")
		}

		feeCoins := feeTx.GetFee()
		gas := feeTx.GetGas()

		// Read consensus-level minimum gas price from chain state.
		var minGasPriceNgonka uint64
		if inferenceKeeper != nil {
			fp := inferenceKeeper.GetFeeParams(ctx)
			minGasPriceNgonka = fp.MinGasPriceNgonka
		}

		// If min gas price is 0 (e.g., during genesis or if governance sets it to 0),
		// fall through to accept any fee.
		if minGasPriceNgonka == 0 {
			priority := getTxPriority(feeCoins, gas)
			return feeCoins, priority, nil
		}

		// Calculate required fee using big-int math to avoid uint64 overflow.
		requiredAmount := math.NewIntFromUint64(gas).Mul(math.NewIntFromUint64(minGasPriceNgonka))
		requiredFee := sdk.NewCoin("ngonka", requiredAmount)

		if !feeCoins.IsAnyGTE(sdk.NewCoins(requiredFee)) {
			return nil, 0, errorsmod.Wrapf(sdkerrors.ErrInsufficientFee,
				"insufficient fee: got %s, required at least %s (gas=%d, min_gas_price=%dngonka)",
				feeCoins, requiredFee, gas, minGasPriceNgonka)
		}

		priority := getTxPriority(feeCoins, gas)
		return feeCoins, priority, nil
	}
}

// getTxPriority calculates transaction priority from fee and gas.
// Higher fee per gas = higher priority.
func getTxPriority(feeCoins sdk.Coins, gas uint64) int64 {
	if gas == 0 {
		return 0
	}

	// Clamp gas to max int64 to avoid overflow in QuoRaw.
	const maxInt64 = int64(^uint64(0) >> 1)
	divisor := maxInt64
	if gas <= uint64(maxInt64) {
		divisor = int64(gas)
	}

	var priority int64
	for _, coin := range feeCoins {
		gasPrice := coin.Amount.QuoRaw(divisor)
		amt := gasPrice.Int64()
		if amt > priority {
			priority = amt
		}
	}
	return priority
}
