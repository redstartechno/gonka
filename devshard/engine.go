package devshard

import (
	"context"
	"errors"
)

// ErrValidationAlreadyLeased is returned when another devshardd instance already
// holds the validation lease for an inference.
var ErrValidationAlreadyLeased = errors.New("validation leased by another instance")

// ErrValidationSkipped signals that a validation attempt was deliberately
// abandoned without producing a MsgValidation or MsgValidationVote.
// The canonical trigger is the executor returning 404 for the payload
// (the payload has already been pruned). Callers should treat this as a
// quiet no-op rather than a validation failure.
var ErrValidationSkipped = errors.New("devshard validation skipped")

// InferenceEngine executes inference on an ML node.
// Implemented by dapi using existing broker + completionapi.
type InferenceEngine interface {
	Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error)
}

// ValidationEngine re-executes inference and compares logits.
// Implemented by dapi using existing broker + completionapi.
type ValidationEngine interface {
	Validate(ctx context.Context, req ValidateRequest) (*ValidateResult, error)
}

// ValidationCompletionRecorder can be implemented by validation engines that
// need to persist successful async MsgValidation submission.
type ValidationCompletionRecorder interface {
	MarkValidationSubmitted(ctx context.Context, escrowID string, inferenceID uint64) error
}
