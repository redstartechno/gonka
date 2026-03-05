package user

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"

	"google.golang.org/protobuf/proto"

	"subnet/host"
	"subnet/signing"
	"subnet/state"
	"subnet/types"
)

type HostClient interface {
	Send(ctx context.Context, req host.HostRequest) (*host.HostResponse, error)
}

type InProcessClient struct {
	Host *host.Host
}

func (c *InProcessClient) Send(ctx context.Context, req host.HostRequest) (*host.HostResponse, error) {
	return c.Host.HandleRequest(ctx, req)
}

// InferenceParams describes a new inference to send.
type InferenceParams struct {
	Model       string
	Prompt      []byte
	InputLength uint64
	MaxTokens   uint64
	StartedAt   int64
}

// InferenceResult is the outcome of SendInference.
type InferenceResult struct {
	InferenceID uint64
	Nonce       uint64
	Receipt     []byte          // executor receipt, nil if not received
	StateSig    []byte          // state signature from the contacted host
	Mempool     []*types.SubnetTx
}

// Session manages the user side of the subnet protocol.
type Session struct {
	sm            *state.StateMachine
	signer        signing.Signer
	escrowID      string
	group         []types.SlotAssignment
	clients       []HostClient
	nonce         uint64
	diffs         []types.Diff                 // append-only log
	hostSyncNonce map[int]uint64               // hostIdx -> last nonce sent
	pendingTxs    []*types.SubnetTx            // from host mempools, for next diff
	pendingTxKeys map[string]struct{}           // dedup set keyed by tx_type:id
	signatures    map[uint64]map[uint32][]byte // nonce -> slotID -> sig
}

// NewSession creates a user session. clients must match group length.
func NewSession(
	sm *state.StateMachine,
	signer signing.Signer,
	escrowID string,
	group []types.SlotAssignment,
	clients []HostClient,
) (*Session, error) {
	if len(clients) != len(group) {
		return nil, fmt.Errorf("%w: got %d clients for %d slots",
			types.ErrGroupSizeMismatch, len(clients), len(group))
	}
	return &Session{
		sm:            sm,
		signer:        signer,
		escrowID:      escrowID,
		group:         group,
		clients:       clients,
		hostSyncNonce: make(map[int]uint64),
		pendingTxKeys: make(map[string]struct{}),
		signatures:    make(map[uint64]map[uint32][]byte),
	}, nil
}

// NextDiff composes a diff with pending txs + new MsgStartInference.
// Returns the diff and target host index. Does NOT send.
func (s *Session) NextDiff(params InferenceParams) (types.Diff, int, error) {
	nonce := s.nonce + 1
	hostIdx := int(nonce % uint64(len(s.group)))

	var txs []*types.SubnetTx

	// Include pending txs from host mempools (receipts, finish msgs).
	txs = append(txs, s.pendingTxs...)

	// Add MsgStartInference.
	promptHash := sha256.Sum256(params.Prompt)
	txs = append(txs, &types.SubnetTx{Tx: &types.SubnetTx_StartInference{
		StartInference: &types.MsgStartInference{
			InferenceId: nonce,
			Model:       params.Model,
			PromptHash:  promptHash[:],
			InputLength: params.InputLength,
			MaxTokens:   params.MaxTokens,
			StartedAt:   params.StartedAt,
		},
	}})

	// Sign the diff.
	content := state.BuildDiffContent(nonce, txs)
	data, err := proto.Marshal(content)
	if err != nil {
		return types.Diff{}, 0, fmt.Errorf("marshal diff content: %w", err)
	}
	sig, err := s.signer.Sign(data)
	if err != nil {
		return types.Diff{}, 0, fmt.Errorf("sign diff: %w", err)
	}

	diff := types.Diff{Nonce: nonce, Txs: txs, UserSig: sig}
	return diff, hostIdx, nil
}

// DiffsForHost returns catch-up diffs for a host (from its last sync nonce to current).
func (s *Session) DiffsForHost(hostIdx int) []types.Diff {
	lastSent := s.hostSyncNonce[hostIdx]
	var result []types.Diff
	for _, d := range s.diffs {
		if d.Nonce > lastSent {
			result = append(result, d)
		}
	}
	return result
}

// ProcessResponse updates session state from a host response.
// Queues receipt as MsgConfirmStart, adds mempool txs, stores signature.
// Verifies that the host's state hash matches the local state root.
func (s *Session) ProcessResponse(hostIdx int, resp *host.HostResponse) error {
	// Verify state hash if the host returned one.
	if len(resp.StateHash) > 0 {
		localRoot, err := s.sm.ComputeStateRoot()
		if err != nil {
			return fmt.Errorf("compute local state root: %w", err)
		}
		if !bytes.Equal(localRoot, resp.StateHash) {
			return fmt.Errorf("%w: host %d at nonce %d (local %x, host %x)",
				types.ErrStateHashMismatch, hostIdx, resp.Nonce, localRoot, resp.StateHash)
		}
	}

	// Store signature.
	if resp.StateSig != nil {
		slotID := s.group[hostIdx].SlotID
		if _, ok := s.signatures[resp.Nonce]; !ok {
			s.signatures[resp.Nonce] = make(map[uint32][]byte)
		}
		s.signatures[resp.Nonce][slotID] = resp.StateSig
	}

	// Update sync nonce.
	s.hostSyncNonce[hostIdx] = resp.Nonce

	// Queue receipt as MsgConfirmStart for the next diff.
	if resp.Receipt != nil {
		// The inference ID is the nonce of the diff that triggered the receipt.
		s.addPendingTx(&types.SubnetTx{
			Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
				InferenceId: resp.Nonce,
				ExecutorSig: resp.Receipt,
			}},
		})
	}

	// Queue mempool txs (finish msgs) for the next diff.
	for _, tx := range resp.Mempool {
		s.addPendingTx(tx)
	}

	return nil
}

// SendInference composes diff, sends to correct host, processes response.
func (s *Session) SendInference(ctx context.Context, params InferenceParams) (*InferenceResult, error) {
	diff, hostIdx, err := s.NextDiff(params)
	if err != nil {
		return nil, err
	}

	// Apply locally to validate.
	if _, err := s.sm.ApplyDiff(diff); err != nil {
		return nil, fmt.Errorf("local apply: %w", err)
	}

	// Record the diff and advance nonce.
	s.diffs = append(s.diffs, diff)
	s.nonce = diff.Nonce
	// Clear pending txs since they're now included in this diff.
	s.clearPendingTxs()

	// Build catch-up diffs for the target host.
	catchUpDiffs := s.DiffsForHost(hostIdx)

	resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{
		Diffs: catchUpDiffs,
		Nonce: diff.Nonce,
		Payload: &host.InferencePayload{
			Prompt:      params.Prompt,
			Model:       params.Model,
			InputLength: params.InputLength,
			MaxTokens:   params.MaxTokens,
			StartedAt:   params.StartedAt,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("send to host %d: %w", hostIdx, err)
	}

	if err := s.ProcessResponse(hostIdx, resp); err != nil {
		return nil, fmt.Errorf("process response from host %d: %w", hostIdx, err)
	}

	return &InferenceResult{
		InferenceID: diff.Nonce,
		Nonce:       resp.Nonce,
		Receipt:     resp.Receipt,
		StateSig:    resp.StateSig,
		Mempool:     resp.Mempool,
	}, nil
}

// Finalize completes the round in two single-pass phases.
//
// Phase A: one diff per host. The first diff (i=0) carries MsgFinalizeRound
// plus any pending txs. Each subsequent diff carries txs returned by the
// previous host's response.
//
// Phase B: one empty diff per host. Propagates complete state via catch-up
// and collects signatures from every host.
func (s *Session) Finalize(ctx context.Context) error {
	n := len(s.group)

	// Phase A: collect remaining txs, one diff per host.
	for i := 0; i < n; i++ {
		nonce := s.nonce + 1
		hostIdx := int(nonce % uint64(n))

		txs := make([]*types.SubnetTx, len(s.pendingTxs))
		copy(txs, s.pendingTxs)
		if i == 0 {
			txs = append(txs, &types.SubnetTx{Tx: &types.SubnetTx_FinalizeRound{
				FinalizeRound: &types.MsgFinalizeRound{},
			}})
		}

		diff, err := s.signDiff(nonce, txs)
		if err != nil {
			return err
		}
		if _, err := s.sm.ApplyDiff(diff); err != nil {
			return fmt.Errorf("local apply: %w", err)
		}
		s.diffs = append(s.diffs, diff)
		s.nonce = nonce
		s.clearPendingTxs()

		if err := s.sendAndProcess(ctx, hostIdx); err != nil {
			return err
		}
	}

	// Phase B: propagate complete state, collect signatures.
	for i := 0; i < n; i++ {
		nonce := s.nonce + 1
		hostIdx := int(nonce % uint64(n))

		diff, err := s.signDiff(nonce, nil)
		if err != nil {
			return err
		}
		if _, err := s.sm.ApplyDiff(diff); err != nil {
			return fmt.Errorf("local apply: %w", err)
		}
		s.diffs = append(s.diffs, diff)
		s.nonce = nonce

		if err := s.sendAndProcess(ctx, hostIdx); err != nil {
			return err
		}
	}

	return nil
}


// signDiff builds and signs a diff with the given nonce and txs.
func (s *Session) signDiff(nonce uint64, txs []*types.SubnetTx) (types.Diff, error) {
	content := state.BuildDiffContent(nonce, txs)
	data, err := proto.Marshal(content)
	if err != nil {
		return types.Diff{}, fmt.Errorf("marshal diff content: %w", err)
	}
	sig, err := s.signer.Sign(data)
	if err != nil {
		return types.Diff{}, fmt.Errorf("sign diff: %w", err)
	}
	return types.Diff{Nonce: nonce, Txs: txs, UserSig: sig}, nil
}

// sendAndProcess sends catch-up diffs to a host and processes the response.
func (s *Session) sendAndProcess(ctx context.Context, hostIdx int) error {
	catchUp := s.DiffsForHost(hostIdx)
	resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{Diffs: catchUp, Nonce: s.nonce})
	if err != nil {
		return fmt.Errorf("send to host %d: %w", hostIdx, err)
	}
	if err := s.ProcessResponse(hostIdx, resp); err != nil {
		return fmt.Errorf("process response from host %d: %w", hostIdx, err)
	}
	return nil
}

// subnetTxKey returns a dedup key for host-proposed txs.
// Returns "" for user-proposed types (start, finalize, timeout).
func subnetTxKey(tx *types.SubnetTx) string {
	switch inner := tx.GetTx().(type) {
	case *types.SubnetTx_FinishInference:
		return fmt.Sprintf("finish:%d", inner.FinishInference.InferenceId)
	case *types.SubnetTx_ConfirmStart:
		return fmt.Sprintf("confirm:%d", inner.ConfirmStart.InferenceId)
	case *types.SubnetTx_Validation:
		return fmt.Sprintf("validation:%d:%d", inner.Validation.InferenceId, inner.Validation.ValidatorSlot)
	case *types.SubnetTx_ValidationVote:
		return fmt.Sprintf("vote:%d:%d", inner.ValidationVote.InferenceId, inner.ValidationVote.VoterSlot)
	case *types.SubnetTx_RevealSeed:
		return fmt.Sprintf("reveal_seed:%d", inner.RevealSeed.SlotId)
	default:
		return ""
	}
}

// addPendingTx appends tx to pendingTxs if not a duplicate.
func (s *Session) addPendingTx(tx *types.SubnetTx) {
	key := subnetTxKey(tx)
	if key != "" {
		if _, dup := s.pendingTxKeys[key]; dup {
			return
		}
		s.pendingTxKeys[key] = struct{}{}
	}
	s.pendingTxs = append(s.pendingTxs, tx)
}

// clearPendingTxs resets the pending tx slice and dedup set.
func (s *Session) clearPendingTxs() {
	s.pendingTxs = nil
	clear(s.pendingTxKeys)
}

func (s *Session) Signatures() map[uint64]map[uint32][]byte {
	return s.signatures
}

func (s *Session) Nonce() uint64 {
	return s.nonce
}

func (s *Session) Diffs() []types.Diff {
	return s.diffs
}

func (s *Session) PendingTxs() []*types.SubnetTx {
	return s.pendingTxs
}

func (s *Session) StateMachine() *state.StateMachine {
	return s.sm
}

// TimeoutVerifier contacts a host for timeout verification votes.
type TimeoutVerifier interface {
	VerifyTimeout(ctx context.Context, inferenceID uint64, reason types.TimeoutReason, promptData []byte) (accept bool, sig []byte, voterSlot uint32, err error)
}

// CollectTimeoutVotes contacts non-executor hosts to collect signed votes.
// Returns votes for inclusion in MsgTimeoutInference.
func (s *Session) CollectTimeoutVotes(
	ctx context.Context,
	inferenceID uint64,
	reason types.TimeoutReason,
	promptData []byte,
	verifiers map[int]TimeoutVerifier, // hostIdx -> verifier
) ([]*types.TimeoutVote, error) {
	// Determine executor slot.
	executorIdx := int(inferenceID % uint64(len(s.group)))

	type voteResult struct {
		vote *types.TimeoutVote
		err  error
	}

	results := make(chan voteResult, len(verifiers))
	for idx, v := range verifiers {
		if idx == executorIdx {
			continue // skip executor
		}
		go func(verifier TimeoutVerifier) {
			accept, sig, voterSlot, err := verifier.VerifyTimeout(ctx, inferenceID, reason, promptData)
			if err != nil {
				results <- voteResult{err: err}
				return
			}
			if !accept {
				results <- voteResult{} // nil vote, no error
				return
			}
			results <- voteResult{vote: &types.TimeoutVote{
				VoterSlot: voterSlot,
				Accept:    true,
				Signature: sig,
			}}
		}(v)
	}

	var votes []*types.TimeoutVote
	// Count expected responses (non-executor verifiers).
	expected := 0
	for idx := range verifiers {
		if idx != executorIdx {
			expected++
		}
	}

	voteThreshold := s.sm.SnapshotState().Config.VoteThreshold
	for i := 0; i < expected; i++ {
		res := <-results
		if res.err != nil {
			continue // skip failed hosts
		}
		if res.vote != nil {
			votes = append(votes, res.vote)
		}
		// Check if we have enough.
		if uint32(len(votes)) > voteThreshold {
			break
		}
	}

	return votes, nil
}
