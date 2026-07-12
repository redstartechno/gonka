package inference

import (
	"bytes"
	"common/chain"
	"common/completionapi"
	commonvalidation "common/validation"
	"context"
	devshardpkg "devshard"
	"devshard/bridge"
	"devshard/logging"
	"devshard/observability"
	"devshard/storage"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/productscience/inference/x/inference/types"
)

// leaseOps is satisfied by storage.LeaseStore; extracted as interface for testing.
type leaseOps interface {
	Acquire(ctx context.Context, escrowId string, inferenceId uint64, epochId uint64, instanceAddr string) (bool, error)
	SetResult(ctx context.Context, escrowId string, inferenceId uint64, status storage.LeaseStatus) error
}

// Validator implements devshard.ValidationEngine for the standalone devshardd binary.
// It performs ML-based inference validation without lease deduplication.
// Use LeaseValidator to add Postgres-based lease deduplication on top.
type Validator struct {
	bridge       bridge.MainnetBridge
	recorder     PayloadAuthClient
	engine       *Engine
	phase        *chain.Phase
	boundVersion string
	chainParams  ChainParamsProvider
	thresholds   ValidationThresholdResolver
}

// NewValidator creates a Validator. boundVersion is the runtime version string used
// to construct the payload request path. thresholds resolves the per-model
// similarity pass threshold (long-poll snapshot first, chain fallback).
func NewValidator(
	br bridge.MainnetBridge,
	recorder PayloadAuthClient,
	engine *Engine,
	phase *chain.Phase,
	boundVersion string,
	chainParams ChainParamsProvider,
	thresholds ValidationThresholdResolver,
) *Validator {
	return &Validator{
		bridge:       br,
		recorder:     recorder,
		engine:       engine,
		phase:        phase,
		boundVersion: boundVersion,
		chainParams:  chainParams,
		thresholds:   thresholds,
	}
}

func (v *Validator) Validate(ctx context.Context, req devshardpkg.ValidateRequest) (*devshardpkg.ValidateResult, error) {
	inferenceID := strconv.FormatUint(req.InferenceID, 10)

	epochID := req.EpochID
	if epochID == 0 {
		epochID = v.phase.EpochID()
	}
	promptPayload, responsePayload, err := fetchPayloadsFromExecutor(
		ctx, v.bridge, v.recorder, req, inferenceID, epochID, devshardpkg.VersionedSessionPayloadPath(v.boundVersion, req.EscrowID),
	)
	if err != nil {
		if errors.Is(err, commonvalidation.ErrPayloadGone) {
			logging.Info("devshard validation skipped: payload pruned on executor",
				types.Validation,
				"inferenceId", inferenceID,
				"executor", req.ExecutorAddress,
				"epoch", epochID,
			)
			return nil, fmt.Errorf("%w: %v", devshardpkg.ErrValidationSkipped, err)
		}
		return nil, observability.Classify(observability.ReasonPayloadFetchErr, observability.WhereRuntimeValidate, fmt.Errorf("fetch payloads from executor: %w", err))
	}

	if _, err := completionapi.ModifyRequestBodyWithLogprobsMode(promptPayload, int32(req.InferenceID), v.chainParams.LogprobsMode()); err != nil {
		return nil, observability.Classify(observability.ReasonValidationBuildErr, observability.WhereRuntimeValidate, fmt.Errorf("modify request body for validation: %w", err))
	}
	if _, err := commonvalidation.UnmarshalResponsePayload(responsePayload); err != nil {
		return nil, observability.Classify(observability.ReasonOriginalParseErr, observability.WhereRuntimeValidate, fmt.Errorf("parse original response: %w", err))
	}

	result, err := commonvalidation.ExecuteValidation(
		ctx,
		inferenceID,
		promptPayload,
		responsePayload,
		func(ctx context.Context, body []byte) (*http.Response, error) {
			return v.executeMLRequest(ctx, req.Model, body)
		},
		req.InputTokens, req.OutputTokens,
		v.chainParams.LogprobsMode(),
	)
	if err != nil {
		return nil, classifyExecuteValidationErr(err)
	}

	valid, err := evaluateValidationResult(ctx, result, epochID, req.Model, v.thresholds)
	if err != nil {
		return nil, observability.Classify(observability.ReasonValidationBuildErr, observability.WhereRuntimeValidate,
			fmt.Errorf("evaluate validation result: %w", err))
	}
	return &devshardpkg.ValidateResult{Valid: valid}, nil
}

// evaluateValidationResult decides pass/fail from a validation outcome. Similarity
// results use the per-model threshold from chain/runtime config; length, token, and
// invalid outcomes fail directly without a threshold lookup.
func evaluateValidationResult(
	ctx context.Context,
	result commonvalidation.ValidationResult,
	epochID uint64,
	model string,
	thresholds ValidationThresholdResolver,
) (bool, error) {
	switch r := result.(type) {
	case *commonvalidation.SimilarityValidationResult:
		threshold, err := thresholds.Resolve(ctx, epochID, model)
		if err != nil {
			return false, err
		}
		return commonvalidation.SimilarityPassesThreshold(r.Value, threshold), nil
	case *commonvalidation.DifferentLengthValidationResult,
		*commonvalidation.DifferentTokensValidationResult,
		*commonvalidation.InvalidInferenceResult:
		return false, nil
	default:
		return false, fmt.Errorf("unknown validation result type %T", result)
	}
}

func (v *Validator) executeMLRequest(ctx context.Context, model string, body []byte) (*http.Response, error) {
	resp, err := v.engine.doWithLockedNode(ctx, observability.PathValidate, model, func(endpoint string) (*http.Response, error) {
		url := endpoint + "/v1/chat/completions"
		httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if reqErr != nil {
			return nil, observability.Classify(observability.ReasonApplicationErr, observability.WhereEngineMLNodeCall, reqErr)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		observability.InjectRequestContext(ctx, httpReq.Header)
		observability.AttachRequestID(httpReq)
		return v.engine.httpClient.Do(httpReq)
	})
	if err != nil {
		return nil, fmt.Errorf("validate inference: %w", err)
	}
	return resp, nil
}

var _ devshardpkg.ValidationEngine = (*Validator)(nil)

// LeaseValidator wraps a ValidationEngine with Postgres-based lease deduplication so
// that only one devshardd instance validates each (escrow_id, inference_id) pair.
// The retry loop uses the inner Validator directly because it already holds the lease.
type LeaseValidator struct {
	validator    devshardpkg.ValidationEngine
	phase        *chain.Phase
	leases       leaseOps
	instanceAddr string
}

// NewLeaseValidator wraps v with Postgres lease deduplication.
func NewLeaseValidator(v devshardpkg.ValidationEngine, phase *chain.Phase, leases leaseOps, instanceAddr string) *LeaseValidator {
	return &LeaseValidator{
		validator:    v,
		phase:        phase,
		leases:       leases,
		instanceAddr: instanceAddr,
	}
}

func (c *LeaseValidator) Validate(ctx context.Context, req devshardpkg.ValidateRequest) (*devshardpkg.ValidateResult, error) {
	epochID := c.phase.EpochID()
	acquired, err := c.leases.Acquire(ctx, req.EscrowID, req.InferenceID, epochID, c.instanceAddr)
	if err != nil {
		slog.Warn("devshardd: validation lease failed",
			"escrow", req.EscrowID, "inference", req.InferenceID, "error", err)
		return nil, fmt.Errorf("acquire validation: %w", err)
	} else if !acquired {
		return nil, devshardpkg.ErrValidationAlreadyLeased
	}

	result, err := c.validator.Validate(ctx, req)
	if err != nil {
		if errors.Is(err, commonvalidation.ErrHashMismatch) {
			// Executor served wrong payload with valid signature: immediate invalidation, no retry.
			slog.Warn("devshardd: hash mismatch — submitting immediate invalidation",
				"escrow", req.EscrowID, "inference", req.InferenceID)
			return &devshardpkg.ValidateResult{Valid: false}, nil
		}
		return nil, err
	}

	return result, nil
}

func (c *LeaseValidator) MarkValidationSubmitted(ctx context.Context, escrowID string, inferenceID uint64) error {
	return c.leases.SetResult(ctx, escrowID, inferenceID, storage.LeaseStatusSubmitted)
}

var _ devshardpkg.ValidationEngine = (*LeaseValidator)(nil)
var _ devshardpkg.ValidationCompletionRecorder = (*LeaseValidator)(nil)
