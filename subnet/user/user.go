package user

import (
	"bytes"
	"context"
	"fmt"
	"sync"

	"google.golang.org/protobuf/proto"

	"subnet"
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
	Receipt     []byte // executor receipt, nil if not received
	ConfirmedAt int64  // executor wall-clock timestamp, 0 if not executor
	StateSig    []byte // state signature from the contacted host
	Mempool     []*types.SubnetTx
}

// Session manages the user side of the subnet protocol.
type Session struct {
	mu            sync.Mutex
	sm            *state.StateMachine
	signer        signing.Signer
	verifier      signing.Verifier
	escrowID      string
	group         []types.SlotAssignment
	addrToSlots   map[string][]uint32          // validator address -> slot IDs
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
	verifier signing.Verifier,
) (*Session, error) {
	if err := types.ValidateGroup(group); err != nil {
		return nil, err
	}
	if len(clients) != len(group) {
		return nil, fmt.Errorf("%w: got %d clients for %d slots",
			types.ErrGroupSizeMismatch, len(clients), len(group))
	}
	addrToSlots := make(map[string][]uint32, len(group))
	for _, s := range group {
		addrToSlots[s.ValidatorAddress] = append(addrToSlots[s.ValidatorAddress], s.SlotID)
	}
	return &Session{
		sm:            sm,
		signer:        signer,
		verifier:      verifier,
		escrowID:      escrowID,
		group:         group,
		addrToSlots:   addrToSlots,
		clients:       clients,
		hostSyncNonce: make(map[int]uint64),
		pendingTxKeys: make(map[string]struct{}),
		signatures:    make(map[uint64]map[uint32][]byte),
	}, nil
}

// composeDiffTxs builds the txs for the next diff (no side effects).
// Caller must hold s.mu.
func (s *Session) composeDiffTxs(params InferenceParams) (uint64, int, []*types.SubnetTx, error) {
	nonce := s.nonce + 1
	hostIdx := int(nonce % uint64(len(s.group)))

	var txs []*types.SubnetTx

	// Include pending txs from host mempools (receipts, finish msgs).
	txs = append(txs, s.pendingTxs...)

	// Add MsgStartInference.
	promptHash, err := subnet.CanonicalPromptHash(params.Prompt)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("canonical prompt hash: %w", err)
	}
	txs = append(txs, &types.SubnetTx{Tx: &types.SubnetTx_StartInference{
		StartInference: &types.MsgStartInference{
			InferenceId: nonce,
			Model:       params.Model,
			PromptHash:  promptHash,
			InputLength: params.InputLength,
			MaxTokens:   params.MaxTokens,
			StartedAt:   params.StartedAt,
		},
	}})

	return nonce, hostIdx, txs, nil
}

// diffsForHost returns catch-up diffs for a host (from its last sync nonce to current).
// Caller must hold s.mu.
func (s *Session) diffsForHost(hostIdx int) []types.Diff {
	lastSent := s.hostSyncNonce[hostIdx]
	var result []types.Diff
	for _, d := range s.diffs {
		if d.Nonce > lastSent {
			result = append(result, d)
		}
	}
	return result
}

// processResponse updates session state from a host response.
// Caller must hold s.mu.
func (s *Session) processResponse(hostIdx int, resp *host.HostResponse) error {
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

	// Verify and store state signature.
	if resp.StateSig != nil {
		expectedAddr := s.group[hostIdx].ValidatorAddress
		sigContent := &types.StateSignatureContent{
			StateRoot: resp.StateHash,
			EscrowId:  s.escrowID,
			Nonce:     resp.Nonce,
		}
		sigData, err := proto.Marshal(sigContent)
		if err != nil {
			return fmt.Errorf("marshal state sig content: %w", err)
		}
		addr, err := s.verifier.RecoverAddress(sigData, resp.StateSig)
		if err != nil {
			return fmt.Errorf("%w: host %d: %v", types.ErrInvalidStateSig, hostIdx, err)
		}
		if addr != expectedAddr {
			if !s.sm.CheckWarmKey(addr, expectedAddr) {
				return fmt.Errorf("%w: host %d: expected %s, got %s",
					types.ErrInvalidStateSig, hostIdx, expectedAddr, addr)
			}
		}

		// Store for all slots owned by this validator address.
		if _, ok := s.signatures[resp.Nonce]; !ok {
			s.signatures[resp.Nonce] = make(map[uint32][]byte)
		}
		for _, slot := range s.addrToSlots[expectedAddr] {
			s.signatures[resp.Nonce][slot] = resp.StateSig
		}
	}

	// Update sync nonce.
	s.hostSyncNonce[hostIdx] = resp.Nonce

	// Queue receipt as MsgConfirmStart for the next diff.
	if resp.Receipt != nil {
		s.addPendingTx(&types.SubnetTx{
			Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
				InferenceId: resp.Nonce,
				ExecutorSig: resp.Receipt,
				ConfirmedAt: resp.ConfirmedAt,
			}},
		})
	}

	// Queue mempool txs (finish msgs) for the next diff.
	for _, tx := range resp.Mempool {
		s.addPendingTx(tx)
	}

	return nil
}

// ProcessResponse updates session state from a host response. Thread-safe.
func (s *Session) ProcessResponse(hostIdx int, resp *host.HostResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.processResponse(hostIdx, resp)
}

// NextDiff composes a diff with pending txs + new MsgStartInference.
// Does NOT apply state or advance nonce (peek-only). Thread-safe.
func (s *Session) NextDiff(params InferenceParams) (types.Diff, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nonce, hostIdx, txs, err := s.composeDiffTxs(params)
	if err != nil {
		return types.Diff{}, 0, err
	}
	diff, err := s.signDiff(nonce, txs, nil)
	if err != nil {
		return types.Diff{}, 0, err
	}
	return diff, hostIdx, nil
}

// preparedInference holds the data prepared under lock for an inference send.
type preparedInference struct {
	diff       types.Diff
	hostIdx    int
	catchUp    []types.Diff
	params     InferenceParams
}

// PrepareInference composes a diff, applies it locally, advances nonce,
// and returns everything needed for the HTTP send. Thread-safe.
func (s *Session) PrepareInference(params InferenceParams) (*preparedInference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nonce, hostIdx, txs, err := s.composeDiffTxs(params)
	if err != nil {
		return nil, err
	}

	// Apply locally to compute post_state_root, then sign.
	postStateRoot, err := s.sm.ApplyLocal(nonce, txs)
	if err != nil {
		return nil, fmt.Errorf("local apply: %w", err)
	}
	diff, err := s.signDiff(nonce, txs, postStateRoot)
	if err != nil {
		return nil, err
	}

	s.diffs = append(s.diffs, diff)
	s.nonce = diff.Nonce
	s.clearPendingTxs()

	catchUp := s.diffsForHost(hostIdx)

	return &preparedInference{
		diff:    diff,
		hostIdx: hostIdx,
		catchUp: catchUp,
		params:  params,
	}, nil
}

// SendPrepared sends a prepared inference to the host and processes the response.
// The HTTP send runs without holding the lock; response processing re-acquires it.
func (s *Session) SendPrepared(ctx context.Context, p *preparedInference) (*InferenceResult, error) {
	resp, err := s.clients[p.hostIdx].Send(ctx, host.HostRequest{
		Diffs: p.catchUp,
		Nonce: p.diff.Nonce,
		Payload: &host.InferencePayload{
			Prompt:      p.params.Prompt,
			Model:       p.params.Model,
			InputLength: p.params.InputLength,
			MaxTokens:   p.params.MaxTokens,
			StartedAt:   p.params.StartedAt,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("send to host %d: %w", p.hostIdx, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.processResponse(p.hostIdx, resp); err != nil {
		return nil, fmt.Errorf("process response from host %d: %w", p.hostIdx, err)
	}

	return &InferenceResult{
		InferenceID: p.diff.Nonce,
		Nonce:       resp.Nonce,
		Receipt:     resp.Receipt,
		ConfirmedAt: resp.ConfirmedAt,
		StateSig:    resp.StateSig,
		Mempool:     resp.Mempool,
	}, nil
}

// SendInference composes diff, sends to correct host, processes response.
// Convenience wrapper around PrepareInference + SendPrepared.
func (s *Session) SendInference(ctx context.Context, params InferenceParams) (*InferenceResult, error) {
	p, err := s.PrepareInference(params)
	if err != nil {
		return nil, err
	}
	return s.SendPrepared(ctx, p)
}

// Finalize completes the round in three phases.
//
// Phase A (N iterations): The first diff carries MsgFinalizeRound plus any
// pending txs. Each subsequent diff carries txs returned by the previous
// host's response. Hosts see Finalizing for the first time and produce
// MsgRevealSeed in their mempool.
//
// Phase A+1 (1 iteration): Drains the last host's MsgRevealSeed that
// remained in pendingTxs after Phase A. This is the final nonce that
// carries any txs. After this, state is frozen.
//
// Phase B (N iterations): Pure propagation + signature collection. No new
// diffs created. Sends catch-up diffs so every host reaches the final
// nonce and signs the same state.
func (s *Session) Finalize(ctx context.Context) error {
	n := len(s.group)

	// Phase A: collect remaining txs, one diff per host.
	for i := 0; i < n; i++ {
		s.mu.Lock()
		nonce := s.nonce + 1
		hostIdx := int(nonce % uint64(n))

		s.filterPendingTxs()
		txs := make([]*types.SubnetTx, len(s.pendingTxs))
		copy(txs, s.pendingTxs)
		if i == 0 {
			txs = append(txs, &types.SubnetTx{Tx: &types.SubnetTx_FinalizeRound{
				FinalizeRound: &types.MsgFinalizeRound{},
			}})
		}

		postStateRoot, err := s.sm.ApplyLocal(nonce, txs)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("local apply: %w", err)
		}
		diff, err := s.signDiff(nonce, txs, postStateRoot)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.diffs = append(s.diffs, diff)
		s.nonce = nonce
		s.clearPendingTxs()
		catchUp := s.diffsForHost(hostIdx)
		s.mu.Unlock()

		resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{Diffs: catchUp, Nonce: nonce})
		if err != nil {
			continue // skip dead host
		}

		s.mu.Lock()
		if err := s.processResponse(hostIdx, resp); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("process response from host %d: %w", hostIdx, err)
		}
		s.mu.Unlock()
	}

	// Phase A+1: drain the last host's reveal sitting in pendingTxs.
	{
		s.mu.Lock()
		nonce := s.nonce + 1
		hostIdx := int(nonce % uint64(n))

		s.filterPendingTxs()
		txs := make([]*types.SubnetTx, len(s.pendingTxs))
		copy(txs, s.pendingTxs)

		postStateRoot, err := s.sm.ApplyLocal(nonce, txs)
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("local apply: %w", err)
		}
		diff, err := s.signDiff(nonce, txs, postStateRoot)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		s.diffs = append(s.diffs, diff)
		s.nonce = nonce
		s.clearPendingTxs()
		catchUp := s.diffsForHost(hostIdx)
		s.mu.Unlock()

		resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{Diffs: catchUp, Nonce: nonce})
		if err != nil {
			// skip dead host in A+1
		} else {
			s.mu.Lock()
			if err := s.processResponse(hostIdx, resp); err != nil {
				s.mu.Unlock()
				return fmt.Errorf("process response from host %d: %w", hostIdx, err)
			}
			s.mu.Unlock()
		}
	}

	// Phase B: propagate complete state, collect signatures.
	// Nonce is frozen -- no new diffs. Each host receives catch-up to
	// the final nonce and signs the same state.
	for hostIdx := 0; hostIdx < n; hostIdx++ {
		s.mu.Lock()
		nonce := s.nonce
		catchUp := s.diffsForHost(hostIdx)
		s.mu.Unlock()

		resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{Diffs: catchUp, Nonce: nonce})
		if err != nil {
			continue // skip dead host
		}

		s.mu.Lock()
		if err := s.processResponse(hostIdx, resp); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("process response from host %d: %w", hostIdx, err)
		}
		s.mu.Unlock()
	}

	// Check signature quorum: need 2/3+1 slot-weighted signatures.
	var sigWeight uint32
	finalNonce := s.nonce
	if sigs, ok := s.signatures[finalNonce]; ok {
		counted := make(map[string]bool)
		for slotID := range sigs {
			addr := s.sm.SlotAddress(slotID)
			if counted[addr] {
				continue
			}
			counted[addr] = true
			sigWeight += s.sm.AddressSlotCount(addr)
		}
	}
	threshold := 2*s.sm.TotalSlots()/3 + 1
	if sigWeight < threshold {
		return fmt.Errorf("insufficient signatures: %d/%d weight", sigWeight, threshold)
	}

	return nil
}


// signDiff builds and signs a diff with the given nonce, txs, and post_state_root.
func (s *Session) signDiff(nonce uint64, txs []*types.SubnetTx, postStateRoot []byte) (types.Diff, error) {
	content := state.BuildDiffContent(s.escrowID, nonce, txs, postStateRoot)
	data, err := proto.Marshal(content)
	if err != nil {
		return types.Diff{}, fmt.Errorf("marshal diff content: %w", err)
	}
	sig, err := s.signer.Sign(data)
	if err != nil {
		return types.Diff{}, fmt.Errorf("sign diff: %w", err)
	}
	return types.Diff{Nonce: nonce, Txs: txs, UserSig: sig, PostStateRoot: postStateRoot}, nil
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

// filterPendingTxs removes txs that would be rejected by the state machine.
// Currently: drops MsgRevealSeed for addresses that already revealed.
func (s *Session) filterPendingTxs() {
	seeds := s.sm.RevealedSlots()
	if len(seeds) == 0 {
		return
	}

	// Build set of addresses that already revealed.
	revealed := make(map[string]bool, len(seeds))
	for slot := range seeds {
		revealed[s.sm.SlotAddress(slot)] = true
	}

	filtered := s.pendingTxs[:0]
	for _, tx := range s.pendingTxs {
		if rs := tx.GetRevealSeed(); rs != nil {
			addr := s.sm.SlotAddress(rs.SlotId)
			if revealed[addr] {
				// Drop: already revealed.
				key := subnetTxKey(tx)
				if key != "" {
					delete(s.pendingTxKeys, key)
				}
				continue
			}
		}
		filtered = append(filtered, tx)
	}
	s.pendingTxs = filtered
}

func (s *Session) Signatures() map[uint64]map[uint32][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.signatures
}

func (s *Session) Nonce() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nonce
}

func (s *Session) Diffs() []types.Diff {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.diffs
}

func (s *Session) PendingTxs() []*types.SubnetTx {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingTxs
}

func (s *Session) StateMachine() *state.StateMachine { return s.sm }

// TimeoutVerifier contacts a host for timeout verification votes.
type TimeoutVerifier interface {
	VerifyTimeout(ctx context.Context, inferenceID uint64, reason types.TimeoutReason, payload *host.InferencePayload) (accept bool, sig []byte, voterSlot uint32, err error)
}

// CollectTimeoutVotes contacts non-executor hosts to collect signed votes.
// Returns votes for inclusion in MsgTimeoutInference.
// Deduplicates verifiers by validator address to avoid duplicate votes
// when the same validator occupies multiple slots.
func (s *Session) CollectTimeoutVotes(
	ctx context.Context,
	inferenceID uint64,
	reason types.TimeoutReason,
	payload *host.InferencePayload,
	verifiers map[int]TimeoutVerifier, // hostIdx -> verifier
) ([]*types.TimeoutVote, error) {
	// Determine executor slot and resolve its validator address.
	executorIdx := int(inferenceID % uint64(len(s.group)))
	executorAddr := s.group[executorIdx].ValidatorAddress

	// Dedup verifiers by address to avoid duplicate votes from multi-slot validators.
	// Pre-seed the executor's address so ALL slots owned by that validator are excluded,
	// not just the single executor index. This prevents a multi-slot executor from
	// voting on its own timeout through a different slot.
	type addrVerifier struct {
		idx      int
		verifier TimeoutVerifier
	}
	seen := make(map[string]bool)
	seen[executorAddr] = true
	var deduped []addrVerifier
	for idx, v := range verifiers {
		addr := s.group[idx].ValidatorAddress
		if seen[addr] {
			continue
		}
		seen[addr] = true
		deduped = append(deduped, addrVerifier{idx, v})
	}

	type voteResult struct {
		vote *types.TimeoutVote
		err  error
	}

	results := make(chan voteResult, len(deduped))
	for _, av := range deduped {
		go func(verifier TimeoutVerifier) {
			accept, sig, voterSlot, err := verifier.VerifyTimeout(ctx, inferenceID, reason, payload)
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
		}(av.verifier)
	}

	var votes []*types.TimeoutVote
	expected := len(deduped)

	voteThreshold := s.sm.VoteThreshold()
	var accWeight uint32
	for i := 0; i < expected; i++ {
		res := <-results
		if res.err != nil {
			continue // skip failed hosts
		}
		if res.vote != nil {
			votes = append(votes, res.vote)
			voterAddr := s.sm.SlotAddress(res.vote.VoterSlot)
			accWeight += s.sm.AddressSlotCount(voterAddr)
		}
		if accWeight > voteThreshold {
			break
		}
	}

	return votes, nil
}
