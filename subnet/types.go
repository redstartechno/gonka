package subnet

// ExecuteRequest contains the data needed to execute an inference.
type ExecuteRequest struct {
	InferenceID uint64
	Model       string
	Prompt      []byte
	PromptHash  []byte
	InputLength uint64
	MaxTokens   uint64
	EscrowID    string // Session escrow ID for namespaced payload storage
}

// ExecuteResult contains the outcome of an inference execution.
type ExecuteResult struct {
	ResponseHash []byte
	InputTokens  uint64
	OutputTokens uint64
}

// ValidateRequest contains the data needed to validate an inference.
type ValidateRequest struct {
	InferenceID  uint64
	Model        string
	PromptHash   []byte
	ResponseHash []byte
	InputTokens  uint64
	OutputTokens uint64

	// Fields for remote payload retrieval (subnet validation)
	EscrowID        string   // Session escrow ID for building the payload URL path
	EpochID         uint64   // Epoch when the executor stored the payload
	ExecutorAddress string   // Executor's validator address for signature verification
	ExecutorPubKeys [][]byte // Executor's raw pubkeys (cold key + warm keys) for signature verification
}

// ValidateResult contains the outcome of a validation.
type ValidateResult struct {
	Valid bool
}
