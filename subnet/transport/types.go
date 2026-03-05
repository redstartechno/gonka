package transport

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	"subnet/host"
	"subnet/types"
)

// DiffJSON is the JSON wire format for a single diff.
// Proto-serialized fields travel as base64 to preserve signature integrity.
type DiffJSON struct {
	Nonce   uint64 `json:"nonce"`
	Txs     []byte `json:"txs"`      // proto bytes of DiffContent.Txs wrapper
	UserSig []byte `json:"user_sig"` // raw sig bytes
}

// PayloadJSON is the JSON wire format for inference payload.
type PayloadJSON struct {
	Prompt      []byte `json:"prompt"`
	Model       string `json:"model"`
	InputLength uint64 `json:"input_length"`
	MaxTokens   uint64 `json:"max_tokens"`
	StartedAt   int64  `json:"started_at"`
}

// InferenceRequest is the JSON body for POST /sessions/:id/chat/completions.
type InferenceRequest struct {
	Diffs   []DiffJSON   `json:"diffs"`
	Nonce   uint64       `json:"nonce"`
	Payload *PayloadJSON `json:"payload,omitempty"`
}

// InferenceResponse is the JSON body returned by the inference endpoint.
type InferenceResponse struct {
	StateSig  []byte   `json:"state_sig,omitempty"`
	StateHash []byte   `json:"state_hash,omitempty"`
	Nonce     uint64   `json:"nonce"`
	Receipt   []byte   `json:"receipt,omitempty"`
	Mempool   [][]byte `json:"mempool,omitempty"` // each: proto bytes of SubnetTx
}

// VerifyTimeoutRequest is the JSON body for POST /sessions/:id/verify-timeout.
type VerifyTimeoutRequest struct {
	InferenceID uint64 `json:"inference_id"`
	Reason      string `json:"reason"` // "refused" or "execution"
	PromptData  []byte `json:"prompt_data,omitempty"`
}

// VerifyTimeoutResponse is returned by the timeout verification endpoint.
type VerifyTimeoutResponse struct {
	Accept    bool   `json:"accept"`
	Signature []byte `json:"signature,omitempty"` // signed TimeoutVoteContent
	VoterSlot uint32 `json:"voter_slot"`
}

// GossipNonceRequest is the JSON body for POST /sessions/:id/gossip/nonce.
type GossipNonceRequest struct {
	Nonce     uint64 `json:"nonce"`
	StateHash []byte `json:"state_hash"`
	StateSig  []byte `json:"state_sig"`
	SlotID    uint32 `json:"slot_id"`
}

// GossipTxsRequest is the JSON body for POST /sessions/:id/gossip/txs.
type GossipTxsRequest struct {
	Txs [][]byte `json:"txs"` // each: proto bytes of SubnetTx
}

// DiffToJSON converts a domain Diff to its JSON wire format.
func DiffToJSON(d types.Diff) (DiffJSON, error) {
	// Serialize the txs as a DiffContent proto (nonce + txs together)
	// to preserve the exact bytes that were signed.
	content := &types.DiffContent{Nonce: d.Nonce, Txs: d.Txs}
	txsBytes, err := proto.Marshal(content)
	if err != nil {
		return DiffJSON{}, fmt.Errorf("marshal diff content: %w", err)
	}
	return DiffJSON{
		Nonce:   d.Nonce,
		Txs:     txsBytes,
		UserSig: d.UserSig,
	}, nil
}

// DiffFromJSON converts a JSON wire diff back to the domain Diff.
func DiffFromJSON(dj DiffJSON) (types.Diff, error) {
	var content types.DiffContent
	if err := proto.Unmarshal(dj.Txs, &content); err != nil {
		return types.Diff{}, fmt.Errorf("unmarshal diff content: %w", err)
	}
	return types.Diff{
		Nonce:   dj.Nonce,
		Txs:     content.Txs,
		UserSig: dj.UserSig,
	}, nil
}

// HostRequestToJSON converts a HostRequest to InferenceRequest.
func HostRequestToJSON(req host.HostRequest) (InferenceRequest, error) {
	diffs := make([]DiffJSON, len(req.Diffs))
	for i, d := range req.Diffs {
		dj, err := DiffToJSON(d)
		if err != nil {
			return InferenceRequest{}, fmt.Errorf("diff %d: %w", i, err)
		}
		diffs[i] = dj
	}

	ir := InferenceRequest{
		Diffs: diffs,
		Nonce: req.Nonce,
	}
	if req.Payload != nil {
		ir.Payload = &PayloadJSON{
			Prompt:      req.Payload.Prompt,
			Model:       req.Payload.Model,
			InputLength: req.Payload.InputLength,
			MaxTokens:   req.Payload.MaxTokens,
			StartedAt:   req.Payload.StartedAt,
		}
	}
	return ir, nil
}

// HostRequestFromJSON converts an InferenceRequest back to HostRequest.
func HostRequestFromJSON(ir InferenceRequest) (host.HostRequest, error) {
	diffs := make([]types.Diff, len(ir.Diffs))
	for i, dj := range ir.Diffs {
		d, err := DiffFromJSON(dj)
		if err != nil {
			return host.HostRequest{}, fmt.Errorf("diff %d: %w", i, err)
		}
		diffs[i] = d
	}

	req := host.HostRequest{
		Diffs: diffs,
		Nonce: ir.Nonce,
	}
	if ir.Payload != nil {
		req.Payload = &host.InferencePayload{
			Prompt:      ir.Payload.Prompt,
			Model:       ir.Payload.Model,
			InputLength: ir.Payload.InputLength,
			MaxTokens:   ir.Payload.MaxTokens,
			StartedAt:   ir.Payload.StartedAt,
		}
	}
	return req, nil
}

// HostResponseToJSON converts a HostResponse to InferenceResponse.
func HostResponseToJSON(resp *host.HostResponse) (InferenceResponse, error) {
	var mempool [][]byte
	for _, tx := range resp.Mempool {
		b, err := proto.Marshal(tx)
		if err != nil {
			return InferenceResponse{}, fmt.Errorf("marshal mempool tx: %w", err)
		}
		mempool = append(mempool, b)
	}
	return InferenceResponse{
		StateSig:  resp.StateSig,
		StateHash: resp.StateHash,
		Nonce:     resp.Nonce,
		Receipt:   resp.Receipt,
		Mempool:   mempool,
	}, nil
}

// HostResponseFromJSON converts an InferenceResponse back to HostResponse.
func HostResponseFromJSON(ir InferenceResponse) (*host.HostResponse, error) {
	var mempool []*types.SubnetTx
	for i, b := range ir.Mempool {
		tx := &types.SubnetTx{}
		if err := proto.Unmarshal(b, tx); err != nil {
			return nil, fmt.Errorf("unmarshal mempool tx %d: %w", i, err)
		}
		mempool = append(mempool, tx)
	}
	return &host.HostResponse{
		StateSig:  ir.StateSig,
		StateHash: ir.StateHash,
		Nonce:     ir.Nonce,
		Receipt:   ir.Receipt,
		Mempool:   mempool,
	}, nil
}

// SubnetTxsToBytes serializes a slice of SubnetTx to proto byte slices.
func SubnetTxsToBytes(txs []*types.SubnetTx) ([][]byte, error) {
	result := make([][]byte, len(txs))
	for i, tx := range txs {
		b, err := proto.Marshal(tx)
		if err != nil {
			return nil, fmt.Errorf("marshal tx %d: %w", i, err)
		}
		result[i] = b
	}
	return result, nil
}

// SubnetTxsFromBytes deserializes proto byte slices to SubnetTx.
func SubnetTxsFromBytes(data [][]byte) ([]*types.SubnetTx, error) {
	result := make([]*types.SubnetTx, len(data))
	for i, b := range data {
		tx := &types.SubnetTx{}
		if err := proto.Unmarshal(b, tx); err != nil {
			return nil, fmt.Errorf("unmarshal tx %d: %w", i, err)
		}
		result[i] = tx
	}
	return result, nil
}

// TimeoutReasonToString converts proto enum to wire string.
func TimeoutReasonToString(r types.TimeoutReason) string {
	switch r {
	case types.TimeoutReason_TIMEOUT_REASON_REFUSED:
		return "refused"
	case types.TimeoutReason_TIMEOUT_REASON_EXECUTION:
		return "execution"
	default:
		return "unknown"
	}
}

// TimeoutReasonFromString converts wire string to proto enum.
func TimeoutReasonFromString(s string) (types.TimeoutReason, error) {
	switch s {
	case "refused":
		return types.TimeoutReason_TIMEOUT_REASON_REFUSED, nil
	case "execution":
		return types.TimeoutReason_TIMEOUT_REASON_EXECUTION, nil
	default:
		return 0, fmt.Errorf("unknown timeout reason: %s", s)
	}
}
