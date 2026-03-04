package host

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"subnet"
	"subnet/signing"
	"subnet/state"
	"subnet/types"
)

// HostRequest carries diffs from the user to a host.
type HostRequest struct {
	Diffs []types.Diff
}

// HostResponse carries the host's reply back to the user.
type HostResponse struct {
	StateSig []byte          // nil = withheld
	Nonce    uint64          // current nonce after applying diffs
	Receipt  []byte          // executor receipt sig, nil if not executor
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
	var receipt []byte

	for _, diff := range req.Diffs {
		// Skip diffs the host has already seen (catch-up).
		currentNonce := h.sm.GetState().LatestNonce
		if diff.Nonce <= currentNonce {
			continue
		}

		if _, err := h.sm.ApplyDiff(diff); err != nil {
			return nil, fmt.Errorf("apply diff nonce %d: %w", diff.Nonce, err)
		}

		// Remove included txs from mempool.
		h.mempool.RemoveIncluded(diff.Txs)

		// Check if this diff contains MsgStartInference and we are the executor.
		for _, tx := range diff.Txs {
			start := tx.GetStartInference()
			if start == nil {
				continue
			}
			executorSlot := h.group[start.InferenceId%uint64(len(h.group))].SlotID
			if !h.slotIDs[executorSlot] {
				continue
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
				PromptHash:  start.PromptHash,
				InputLength: start.InputLength,
				MaxTokens:   start.MaxTokens,
			})
			if err != nil {
				return nil, fmt.Errorf("execute inference %d: %w", start.InferenceId, err)
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
				ProposedAt: diff.Nonce,
			})
		}
	}

	nonce := h.sm.GetState().LatestNonce

	// Sign state root unless mempool has stale entries or acceptance check fails.
	var stateSig []byte
	blocked := false
	if h.checker != nil {
		if err := h.checker.Check(h.sm.GetState()); err != nil {
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
