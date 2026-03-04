package host

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"google.golang.org/protobuf/proto"

	"subnet"
	"subnet/logging"
	"subnet/signing"
	"subnet/state"
	"subnet/types"
)

// InferencePayload carries the actual request data for the current inference.
// The host verifies these against the signed MsgStartInference in the diff.
type InferencePayload struct {
	Prompt      []byte
	Model       string
	InputLength uint64
	MaxTokens   uint64
	StartedAt   int64
}

// HostRequest carries diffs from the user to a host.
type HostRequest struct {
	Diffs   []types.Diff
	Nonce   uint64            // nonce of the current request
	Payload *InferencePayload // nil if no new inference (e.g., Finalize, empty diffs)
}

// HostResponse carries the host's reply back to the user.
type HostResponse struct {
	StateSig []byte // nil = withheld
	Nonce    uint64 // current nonce after applying diffs
	Receipt  []byte // executor receipt sig, nil if not executor
	Mempool  []*types.SubnetTx
}

// AcceptanceChecker is an optional hook that lets the host withhold its
// signature when a diff contains content the host considers unacceptable
// (e.g. suspicious timestamps, insufficient max_cost). Return a non-nil
// error to withhold; nil to allow signing.
type AcceptanceChecker interface {
	Check(st types.EscrowState) error
}

// Host processes user requests: applies diffs, executes inference, signs state.
type Host struct {
	sm       *state.StateMachine
	signer   signing.Signer
	engine   subnet.InferenceEngine
	escrowID string
	slotIDs  map[uint32]bool
	group    []types.SlotAssignment
	mempool  *Mempool
	grace    uint64
	checker  AcceptanceChecker
}

func NewHost(
	sm *state.StateMachine,
	signer signing.Signer,
	engine subnet.InferenceEngine,
	escrowID string,
	group []types.SlotAssignment,
	grace uint64,
	checker AcceptanceChecker,
) (*Host, error) {
	addr := signer.Address()
	slotIDs := make(map[uint32]bool)
	for _, s := range group {
		if s.ValidatorAddress == addr {
			slotIDs[s.SlotID] = true
		}
	}
	if len(slotIDs) == 0 {
		return nil, fmt.Errorf("%w: %s", types.ErrHostNotInGroup, addr)
	}
	return &Host{
		sm:       sm,
		signer:   signer,
		engine:   engine,
		escrowID: escrowID,
		slotIDs:  slotIDs,
		group:    group,
		mempool:  NewMempool(),
		grace:    grace,
		checker:  checker,
	}, nil
}

func (h *Host) StateRoot() ([]byte, error) { return h.sm.ComputeStateRoot() }

func (h *Host) HandleRequest(ctx context.Context, req HostRequest) (*HostResponse, error) {
	// (a) Apply all new diffs.
	for _, diff := range req.Diffs {
		currentNonce := h.sm.LatestNonce()
		if diff.Nonce <= currentNonce {
			continue
		}
		if _, err := h.sm.ApplyDiff(diff); err != nil {
			return nil, fmt.Errorf("apply diff nonce %d: %w", diff.Nonce, err)
		}
		h.mempool.RemoveIncluded(diff.Txs)
	}

	// (b) Executor check at req.Nonce.
	receipt, err := h.executeIfAssigned(ctx, req)
	if err != nil {
		return nil, err
	}

	// (c) State signing + response.
	nonce := h.sm.LatestNonce()

	var stateSig []byte
	blocked := false
	if h.checker != nil {
		if err := h.checker.Check(h.sm.SnapshotState()); err != nil {
			blocked = true
		}
	}
	if !h.mempool.HasStale(nonce, h.grace) && !blocked {
		root, err := h.sm.ComputeStateRoot()
		if err != nil {
			return nil, fmt.Errorf("compute state root: %w", err)
		}
		sigContent := &types.StateSignatureContent{
			StateRoot: root,
			EscrowId:  h.escrowID,
			Nonce:     nonce,
		}
		sigData, err := proto.Marshal(sigContent)
		if err != nil {
			return nil, fmt.Errorf("marshal state sig content: %w", err)
		}
		sig, err := h.signer.Sign(sigData)
		if err != nil {
			return nil, fmt.Errorf("sign state root: %w", err)
		}
		stateSig = sig
	}

	return &HostResponse{
		StateSig: stateSig,
		Nonce:    nonce,
		Receipt:  receipt,
		Mempool:  h.mempool.Txs(),
	}, nil
}

func (h *Host) findDiff(diffs []types.Diff, nonce uint64) *types.Diff {
	for i := range diffs {
		if diffs[i].Nonce == nonce {
			return &diffs[i]
		}
	}
	return nil
}

func (h *Host) executeIfAssigned(ctx context.Context, req HostRequest) ([]byte, error) {
	if req.Payload == nil {
		return nil, nil
	}
	targetDiff := h.findDiff(req.Diffs, req.Nonce)
	if targetDiff == nil {
		return nil, nil
	}

	var receipt []byte
	for _, tx := range targetDiff.Txs {
		start := tx.GetStartInference()
		if start == nil {
			continue
		}
		executorSlot := h.group[start.InferenceId%uint64(len(h.group))].SlotID
		if !h.slotIDs[executorSlot] {
			continue
		}

		// Verify payload matches signed diff.
		if err := h.verifyPayload(req.Payload, start); err != nil {
			return nil, err
		}

		// Sign executor receipt.
		receiptContent := &types.ExecutorReceiptContent{
			InferenceId: start.InferenceId,
			PromptHash:  start.PromptHash,
			Model:       start.Model,
			InputLength: start.InputLength,
			MaxTokens:   start.MaxTokens,
			StartedAt:   start.StartedAt,
		}
		receiptData, err := proto.Marshal(receiptContent)
		if err != nil {
			return nil, fmt.Errorf("marshal executor receipt: %w", err)
		}
		sig, err := h.signer.Sign(receiptData)
		if err != nil {
			return nil, fmt.Errorf("sign executor receipt: %w", err)
		}
		receipt = sig

		// Execute inference.
		result, err := h.engine.Execute(ctx, subnet.ExecuteRequest{
			InferenceID: start.InferenceId,
			Model:       start.Model,
			Prompt:      req.Payload.Prompt,
			PromptHash:  start.PromptHash,
			InputLength: start.InputLength,
			MaxTokens:   start.MaxTokens,
		})
		if err != nil {
			logging.Error("execute failed", "subsystem", "host", "inference_id", start.InferenceId, "error", err)
			break
		}

		// Build MsgFinishInference, sign as proposer, add to mempool.
		finishMsg := &types.MsgFinishInference{
			InferenceId:  start.InferenceId,
			ResponseHash: result.ResponseHash,
			InputTokens:  result.InputTokens,
			OutputTokens: result.OutputTokens,
			ExecutorSlot: executorSlot,
		}
		finishData, err := proto.Marshal(finishMsg)
		if err != nil {
			return nil, fmt.Errorf("marshal finish msg: %w", err)
		}
		proposerSig, err := h.signer.Sign(finishData)
		if err != nil {
			return nil, fmt.Errorf("sign finish msg: %w", err)
		}
		finishMsg.ProposerSig = proposerSig

		h.mempool.Add(MempoolEntry{
			Tx: &types.SubnetTx{Tx: &types.SubnetTx_FinishInference{
				FinishInference: finishMsg,
			}},
			ProposedAt: targetDiff.Nonce,
		})
	}
	return receipt, nil
}

func (h *Host) verifyPayload(p *InferencePayload, start *types.MsgStartInference) error {
	hash := sha256.Sum256(p.Prompt)
	if !bytes.Equal(hash[:], start.PromptHash) {
		return types.ErrPromptHashMismatch
	}
	if p.InputLength != start.InputLength {
		return fmt.Errorf("%w: input_length %d vs %d", types.ErrPayloadMismatch, p.InputLength, start.InputLength)
	}
	if p.MaxTokens != start.MaxTokens {
		return fmt.Errorf("%w: max_tokens %d vs %d", types.ErrPayloadMismatch, p.MaxTokens, start.MaxTokens)
	}
	if p.StartedAt != start.StartedAt {
		return fmt.Errorf("%w: started_at %d vs %d", types.ErrPayloadMismatch, p.StartedAt, start.StartedAt)
	}
	if p.Model != start.Model {
		return fmt.Errorf("%w: model %s vs %s", types.ErrPayloadMismatch, p.Model, start.Model)
	}
	return nil
}
