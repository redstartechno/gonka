package stub

import (
	"context"
	"crypto/sha256"

	"subnet"
)

// InferenceEngine returns fixed values for testing.
type InferenceEngine struct {
	ResponseHash []byte
	InputTokens  uint64
	OutputTokens uint64
}

func NewInferenceEngine() *InferenceEngine {
	h := sha256.Sum256([]byte("stub"))
	return &InferenceEngine{
		ResponseHash: h[:],
		InputTokens:  80,
		OutputTokens: 40,
	}
}

func (e *InferenceEngine) Execute(_ context.Context, _ subnet.ExecuteRequest) (*subnet.ExecuteResult, error) {
	return &subnet.ExecuteResult{
		ResponseHash: e.ResponseHash,
		InputTokens:  e.InputTokens,
		OutputTokens: e.OutputTokens,
	}, nil
}

// ConfigurableEngine allows per-inference overrides for testing with
// varying token counts. Falls back to Default for IDs not in Override.
type ConfigurableEngine struct {
	Default  subnet.ExecuteResult
	Override map[uint64]subnet.ExecuteResult // inference_id -> result
}

func (e *ConfigurableEngine) Execute(_ context.Context, req subnet.ExecuteRequest) (*subnet.ExecuteResult, error) {
	if r, ok := e.Override[req.InferenceID]; ok {
		return &r, nil
	}
	cp := e.Default
	return &cp, nil
}

// ValidationEngine returns fixed validation results for testing.
type ValidationEngine struct {
	Valid bool
}

func NewValidationEngine() *ValidationEngine {
	return &ValidationEngine{Valid: true}
}

func (e *ValidationEngine) Validate(_ context.Context, _ subnet.ValidateRequest) (*subnet.ValidateResult, error) {
	return &subnet.ValidateResult{Valid: e.Valid}, nil
}
