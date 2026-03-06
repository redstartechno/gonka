package state

import (
	"bytes"
	"fmt"
	"maps"
	"slices"

	"google.golang.org/protobuf/proto"

	"subnet/logging"
	"subnet/signing"
	"subnet/types"
)

func safeMul(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	result := a * b
	if result/a != b {
		return 0, false
	}
	return result, true
}

func safeAdd(a, b uint64) (uint64, bool) {
	result := a + b
	if result < a {
		return 0, false
	}
	return result, true
}

// StateMachine applies diffs and tracks session state.
type StateMachine struct {
	state       *types.EscrowState
	verifier    signing.Verifier
	userAddress string

	// Lookup maps derived from group at construction time.
	slotToAddress      map[uint32]string
	slotToPubKey       map[uint32][]byte
	addressInGroup     map[string]bool
	addressToSlotCount map[string]uint32
	totalSlots         uint32
}

func NewStateMachine(
	escrowID string,
	config types.SessionConfig,
	group []types.SlotAssignment,
	balance uint64,
	userAddress string,
	verifier signing.Verifier,
) *StateMachine {
	slotToAddr := make(map[uint32]string, len(group))
	slotToPub := make(map[uint32][]byte, len(group))
	addrInGroup := make(map[string]bool, len(group))
	addrToSlotCount := make(map[string]uint32, len(group))
	for _, s := range group {
		slotToAddr[s.SlotID] = s.ValidatorAddress
		slotToPub[s.SlotID] = s.PublicKey
		addrInGroup[s.ValidatorAddress] = true
		addrToSlotCount[s.ValidatorAddress]++
	}

	groupCopy := make([]types.SlotAssignment, len(group))
	copy(groupCopy, group)

	hostStats := make(map[uint32]*types.HostStats, len(group))
	for _, s := range group {
		hostStats[s.SlotID] = &types.HostStats{}
	}

	return &StateMachine{
		state: &types.EscrowState{
			EscrowID:      escrowID,
			Config:        config,
			Group:         groupCopy,
			Balance:       balance,
			Inferences:    make(map[uint64]*types.InferenceRecord),
			HostStats:     hostStats,
			RevealedSeeds: make(map[uint32]int64),
		},
		verifier:           verifier,
		userAddress:        userAddress,
		slotToAddress:      slotToAddr,
		slotToPubKey:       slotToPub,
		addressInGroup:     addrInGroup,
		addressToSlotCount: addrToSlotCount,
		totalSlots:         uint32(len(group)),
	}
}

// ApplyDiff validates user signature and post_state_root, then applies the diff.
// Returns the computed state root.
func (sm *StateMachine) ApplyDiff(diff types.Diff) ([]byte, error) {
	// 1. Verify user signature (covers nonce, txs, escrow_id, post_state_root).
	diffContent := BuildDiffContent(sm.state.EscrowID, diff.Nonce, diff.Txs, diff.PostStateRoot)
	data, err := proto.Marshal(diffContent)
	if err != nil {
		return nil, fmt.Errorf("marshal diff content: %w", err)
	}

	recovered, err := sm.verifier.RecoverAddress(data, diff.UserSig)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", types.ErrInvalidUserSig, err)
	}
	if recovered != sm.userAddress {
		return nil, fmt.Errorf("%w: expected %s, got %s", types.ErrInvalidUserSig, sm.userAddress, recovered)
	}

	// 2. Apply txs.
	root, err := sm.applyCore(diff.Nonce, diff.Txs)
	if err != nil {
		return nil, err
	}

	// 3. Verify post_state_root if present.
	if len(diff.PostStateRoot) > 0 && !bytes.Equal(root, diff.PostStateRoot) {
		return nil, fmt.Errorf("%w: diff %x, computed %x", types.ErrPostStateRootMismatch, diff.PostStateRoot, root)
	}

	return root, nil
}

// ApplyLocal applies txs without signature verification. Used by the user
// to compute the post_state_root before signing the diff.
func (sm *StateMachine) ApplyLocal(nonce uint64, txs []*types.SubnetTx) ([]byte, error) {
	return sm.applyCore(nonce, txs)
}

// applyCore validates nonce, applies txs, updates nonce, and returns the state root.
func (sm *StateMachine) applyCore(nonce uint64, txs []*types.SubnetTx) ([]byte, error) {
	// 1. Validate nonce.
	expectedNonce := sm.state.LatestNonce + 1
	if nonce != expectedNonce {
		return nil, fmt.Errorf("%w: expected %d, got %d", types.ErrInvalidNonce, expectedNonce, nonce)
	}

	// 2. Validate at most one MsgStartInference per diff, and inference_id == nonce.
	startCount := 0
	for _, tx := range txs {
		if start := tx.GetStartInference(); start != nil {
			startCount++
			if start.InferenceId != nonce {
				return nil, types.ErrInvalidInferenceID
			}
		}
	}
	if startCount > 1 {
		return nil, types.ErrMultipleStartMsgs
	}

	// 3. Snapshot mutable state for rollback on error.
	snap := sm.snapshotMutable()

	// 4. Apply each tx.
	for _, tx := range txs {
		if err := sm.applyTx(tx); err != nil {
			sm.restoreMutable(snap)
			return nil, err
		}
	}

	// 5. Update nonce.
	sm.state.LatestNonce = nonce

	// 6. Compute state root.
	// TODO: optimize for sure
	root, err := ComputeStateRoot(sm.state.Balance, sm.state.HostStats, sm.state.Inferences)
	if err != nil {
		return nil, fmt.Errorf("compute state root: %w", err)
	}

	logging.Debug("applied diff", "subsystem", "state", "nonce", nonce, "txs", len(txs))
	return root, nil
}

// LatestNonce returns the current nonce without deep-copying state.
func (sm *StateMachine) LatestNonce() uint64 {
	return sm.state.LatestNonce
}

// IsFinalizing returns whether the session is in finalizing state.
func (sm *StateMachine) IsFinalizing() bool {
	return sm.state.Finalizing
}

// SnapshotState returns a deep copy of the current escrow state.
func (sm *StateMachine) SnapshotState() types.EscrowState {
	s := *sm.state

	// Deep copy Group.
	s.Group = make([]types.SlotAssignment, len(sm.state.Group))
	for i, sa := range sm.state.Group {
		cp := sa
		if sa.PublicKey != nil {
			cp.PublicKey = make([]byte, len(sa.PublicKey))
			copy(cp.PublicKey, sa.PublicKey)
		}
		s.Group[i] = cp
	}

	// Deep copy HostStats.
	s.HostStats = make(map[uint32]*types.HostStats, len(sm.state.HostStats))
	for k, v := range sm.state.HostStats {
		cp := *v
		s.HostStats[k] = &cp
	}

	// Deep copy RevealedSeeds.
	s.RevealedSeeds = make(map[uint32]int64, len(sm.state.RevealedSeeds))
	maps.Copy(s.RevealedSeeds, sm.state.RevealedSeeds)

	// Deep copy Inferences.
	s.Inferences = make(map[uint64]*types.InferenceRecord, len(sm.state.Inferences))
	for k, v := range sm.state.Inferences {
		cp := *v
		if v.PromptHash != nil {
			cp.PromptHash = make([]byte, len(v.PromptHash))
			copy(cp.PromptHash, v.PromptHash)
		}
		if v.ResponseHash != nil {
			cp.ResponseHash = make([]byte, len(v.ResponseHash))
			copy(cp.ResponseHash, v.ResponseHash)
		}
		if v.VotedSlots != nil {
			cp.VotedSlots = make(map[uint32]bool, len(v.VotedSlots))
			maps.Copy(cp.VotedSlots, v.VotedSlots)
		}
		s.Inferences[k] = &cp
	}

	return s
}

// mutableSnapshot holds the mutable fields of EscrowState for rollback.
type mutableSnapshot struct {
	Balance       uint64
	Finalizing    bool
	Inferences    map[uint64]*types.InferenceRecord
	HostStats     map[uint32]*types.HostStats
	RevealedSeeds map[uint32]int64
}

func (sm *StateMachine) snapshotMutable() mutableSnapshot {
	infCopy := make(map[uint64]*types.InferenceRecord, len(sm.state.Inferences))
	for k, v := range sm.state.Inferences {
		cp := *v
		if v.VotedSlots != nil {
			cp.VotedSlots = make(map[uint32]bool, len(v.VotedSlots))
			maps.Copy(cp.VotedSlots, v.VotedSlots)
		}
		if v.PromptHash != nil {
			cp.PromptHash = make([]byte, len(v.PromptHash))
			copy(cp.PromptHash, v.PromptHash)
		}
		if v.ResponseHash != nil {
			cp.ResponseHash = make([]byte, len(v.ResponseHash))
			copy(cp.ResponseHash, v.ResponseHash)
		}
		infCopy[k] = &cp
	}

	hsCopy := make(map[uint32]*types.HostStats, len(sm.state.HostStats))
	for k, v := range sm.state.HostStats {
		cp := *v
		hsCopy[k] = &cp
	}

	seedsCopy := make(map[uint32]int64, len(sm.state.RevealedSeeds))
	maps.Copy(seedsCopy, sm.state.RevealedSeeds)

	return mutableSnapshot{
		Balance:       sm.state.Balance,
		Finalizing:    sm.state.Finalizing,
		Inferences:    infCopy,
		HostStats:     hsCopy,
		RevealedSeeds: seedsCopy,
	}
}

func (sm *StateMachine) restoreMutable(snap mutableSnapshot) {
	sm.state.Balance = snap.Balance
	sm.state.Finalizing = snap.Finalizing
	sm.state.Inferences = snap.Inferences
	sm.state.HostStats = snap.HostStats
	sm.state.RevealedSeeds = snap.RevealedSeeds
}

// ComputeStateRoot returns the current state root without modifying state.
func (sm *StateMachine) ComputeStateRoot() ([]byte, error) {
	return ComputeStateRoot(sm.state.Balance, sm.state.HostStats, sm.state.Inferences)
}

func (sm *StateMachine) applyTx(tx *types.SubnetTx) error {
	switch inner := tx.GetTx().(type) {
	case *types.SubnetTx_StartInference:
		return sm.applyStartInference(inner.StartInference)
	case *types.SubnetTx_ConfirmStart:
		return sm.applyConfirmStart(inner.ConfirmStart)
	case *types.SubnetTx_FinishInference:
		return sm.applyFinishInference(inner.FinishInference)
	case *types.SubnetTx_Validation:
		return sm.applyValidation(inner.Validation)
	case *types.SubnetTx_ValidationVote:
		return sm.applyValidationVote(inner.ValidationVote)
	case *types.SubnetTx_TimeoutInference:
		return sm.applyTimeout(inner.TimeoutInference)
	case *types.SubnetTx_RevealSeed:
		return sm.applyRevealSeed(inner.RevealSeed)
	case *types.SubnetTx_FinalizeRound:
		return sm.applyFinalizeRound()
	default:
		return types.ErrEmptyTx
	}
}

func (sm *StateMachine) applyStartInference(msg *types.MsgStartInference) error {
	if sm.state.Finalizing {
		return types.ErrSessionFinalizing
	}

	// Duplicate inference ID guard.
	if _, exists := sm.state.Inferences[msg.InferenceId]; exists {
		return types.ErrDuplicateInferenceID
	}

	// Executor slot: group[inference_id % len(group)].SlotID
	executorSlot := sm.state.Group[msg.InferenceId%uint64(len(sm.state.Group))].SlotID

	// Reserve cost: (input_length + max_tokens) * token_price
	sum, ok := safeAdd(msg.InputLength, msg.MaxTokens)
	if !ok {
		return types.ErrCostOverflow
	}
	reservedCost, ok := safeMul(sum, sm.state.Config.TokenPrice)
	if !ok {
		return types.ErrCostOverflow
	}
	if sm.state.Balance < reservedCost {
		return types.ErrInsufficientBalance
	}

	sm.state.Balance -= reservedCost

	rec := &types.InferenceRecord{
		Status:       types.StatusPending,
		ExecutorSlot: executorSlot,
		Model:        msg.Model,
		PromptHash:   msg.PromptHash,
		InputLength:  msg.InputLength,
		MaxTokens:    msg.MaxTokens,
		ReservedCost: reservedCost,
		StartedAt:    msg.StartedAt,
		VotedSlots:   make(map[uint32]bool),
	}

	sm.state.Inferences[msg.InferenceId] = rec
	logging.Debug("new inference", "subsystem", "state", "inference_id", msg.InferenceId, "executor_slot", executorSlot)
	return nil
}

func (sm *StateMachine) applyConfirmStart(msg *types.MsgConfirmStart) error {
	rec, ok := sm.state.Inferences[msg.InferenceId]
	if !ok {
		return fmt.Errorf("%w: inference %d", types.ErrInferenceNotFound, msg.InferenceId)
	}
	if rec.Status != types.StatusPending {
		return fmt.Errorf("%w: expected pending, got %d", types.ErrInvalidTransition, rec.Status)
	}

	// Verify executor receipt (includes confirmed_at from the executor's wall clock).
	receiptContent := &types.ExecutorReceiptContent{
		InferenceId: msg.InferenceId,
		PromptHash:  rec.PromptHash,
		Model:       rec.Model,
		InputLength: rec.InputLength,
		MaxTokens:   rec.MaxTokens,
		StartedAt:   rec.StartedAt,
		EscrowId:    sm.state.EscrowID,
		ConfirmedAt: msg.ConfirmedAt,
	}
	receiptData, err := proto.Marshal(receiptContent)
	if err != nil {
		return fmt.Errorf("marshal executor receipt: %w", err)
	}

	recovered, err := sm.verifier.RecoverAddress(receiptData, msg.ExecutorSig)
	if err != nil {
		return fmt.Errorf("%w: %v", types.ErrInvalidExecutorSig, err)
	}

	expectedAddr := sm.slotToAddress[rec.ExecutorSlot]
	if recovered != expectedAddr {
		return fmt.Errorf("%w: expected executor %s (slot %d), got %s",
			types.ErrInvalidExecutorSig, expectedAddr, rec.ExecutorSlot, recovered)
	}

	rec.Status = types.StatusStarted
	rec.ConfirmedAt = msg.ConfirmedAt
	return nil
}

func (sm *StateMachine) applyFinishInference(msg *types.MsgFinishInference) error {
	rec, ok := sm.state.Inferences[msg.InferenceId]
	if !ok {
		return fmt.Errorf("%w: inference %d", types.ErrInferenceNotFound, msg.InferenceId)
	}
	if rec.Status != types.StatusStarted {
		return fmt.Errorf("%w: expected started, got %d", types.ErrInvalidTransition, rec.Status)
	}

	// Verify executor slot.
	if msg.ExecutorSlot != rec.ExecutorSlot {
		return fmt.Errorf("%w: expected %d, got %d", types.ErrWrongExecutorSlot, rec.ExecutorSlot, msg.ExecutorSlot)
	}

	// Verify proposer signature from executor.
	cloned := proto.Clone(msg).(*types.MsgFinishInference)
	cloned.ProposerSig = nil
	if err := sm.verifyProposerSig(cloned, msg.ProposerSig, sm.slotToAddress[rec.ExecutorSlot]); err != nil {
		return err
	}

	// Compute actual cost.
	tokenSum, ok := safeAdd(msg.InputTokens, msg.OutputTokens)
	if !ok {
		return types.ErrCostOverflow
	}
	actualCost, ok := safeMul(tokenSum, sm.state.Config.TokenPrice)
	if !ok {
		return types.ErrCostOverflow
	}
	if actualCost > rec.ReservedCost {
		return types.ErrActualCostExceedsMax
	}

	// Release surplus.
	surplus := rec.ReservedCost - actualCost
	sm.state.Balance += surplus

	rec.Status = types.StatusFinished
	rec.ResponseHash = msg.ResponseHash
	rec.InputTokens = msg.InputTokens
	rec.OutputTokens = msg.OutputTokens
	rec.ActualCost = actualCost

	// Update host stats.
	sm.state.HostStats[rec.ExecutorSlot].Cost += actualCost

	return nil
}

func (sm *StateMachine) applyValidation(msg *types.MsgValidation) error {
	rec, ok := sm.state.Inferences[msg.InferenceId]
	if !ok {
		return fmt.Errorf("%w: inference %d", types.ErrInferenceNotFound, msg.InferenceId)
	}

	// No-op for terminal states (prevents mempool stall on redundant validations).
	if rec.Status == types.StatusValidated || rec.Status == types.StatusInvalidated {
		return nil
	}

	// Additional validator for already-challenged inference: record in bitmap.
	if rec.Status == types.StatusChallenged {
		if _, ok := sm.slotToAddress[msg.ValidatorSlot]; !ok {
			return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, msg.ValidatorSlot)
		}
		if msg.ValidatorSlot == rec.ExecutorSlot {
			return types.ErrSelfValidation
		}
		// Dedup by address: check if any slot with same address already has bit set.
		validatorAddr := sm.slotToAddress[msg.ValidatorSlot]
		for _, slot := range slices.Sorted(maps.Keys(sm.slotToAddress)) {
			if sm.slotToAddress[slot] == validatorAddr && (rec.ValidatedBy>>slot)&1 == 1 {
				return fmt.Errorf("%w: address %s already validated via slot %d", types.ErrDuplicateValidation, validatorAddr, slot)
			}
		}
		// Verify proposer signature from validator.
		clonedV := proto.Clone(msg).(*types.MsgValidation)
		clonedV.ProposerSig = nil
		if err := sm.verifyProposerSig(clonedV, msg.ProposerSig, sm.slotToAddress[msg.ValidatorSlot]); err != nil {
			return err
		}
		rec.ValidatedBy |= 1 << msg.ValidatorSlot
		return nil
	}

	if rec.Status != types.StatusFinished {
		return fmt.Errorf("%w: expected finished, got %d", types.ErrInvalidTransition, rec.Status)
	}
	if _, ok := sm.slotToAddress[msg.ValidatorSlot]; !ok {
		return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, msg.ValidatorSlot)
	}
	if msg.ValidatorSlot == rec.ExecutorSlot {
		return types.ErrSelfValidation
	}

	// Verify proposer signature from validator.
	clonedV := proto.Clone(msg).(*types.MsgValidation)
	clonedV.ProposerSig = nil
	if err := sm.verifyProposerSig(clonedV, msg.ProposerSig, sm.slotToAddress[msg.ValidatorSlot]); err != nil {
		return err
	}

	// Always transition to Challenged. Terminal states (Validated/Invalidated) are
	// only reached through vote threshold in applyValidationVote.
	// Store the initial validator's judgment for later seed-reveal verification.
	rec.Status = types.StatusChallenged
	rec.ValidatorSlot = msg.ValidatorSlot
	rec.ValidatorValid = msg.Valid
	rec.ValidatedBy |= 1 << msg.ValidatorSlot

	return nil
}

func (sm *StateMachine) applyValidationVote(msg *types.MsgValidationVote) error {
	rec, ok := sm.state.Inferences[msg.InferenceId]
	if !ok {
		return fmt.Errorf("%w: inference %d", types.ErrInferenceNotFound, msg.InferenceId)
	}
	if _, ok := sm.slotToAddress[msg.VoterSlot]; !ok {
		return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, msg.VoterSlot)
	}

	// Skip already-resolved challenge votes (allows safe vote batching).
	if rec.Status == types.StatusValidated || rec.Status == types.StatusInvalidated {
		return nil
	}

	if rec.Status != types.StatusChallenged {
		return fmt.Errorf("%w: expected challenged, got %d", types.ErrInvalidTransition, rec.Status)
	}

	// Dedup by address: a multi-slot validator votes once for all its slots.
	voterAddr := sm.slotToAddress[msg.VoterSlot]
	for _, slot := range slices.Sorted(maps.Keys(rec.VotedSlots)) {
		if rec.VotedSlots[slot] && sm.slotToAddress[slot] == voterAddr {
			return fmt.Errorf("%w: slot %d (address %s already voted via slot %d)", types.ErrDuplicateVote, msg.VoterSlot, voterAddr, slot)
		}
	}

	// Verify proposer signature from voter.
	clonedVV := proto.Clone(msg).(*types.MsgValidationVote)
	clonedVV.ProposerSig = nil
	if err := sm.verifyProposerSig(clonedVV, msg.ProposerSig, sm.slotToAddress[msg.VoterSlot]); err != nil {
		return err
	}

	// Mark ALL slots owned by this address as voted (deterministic dedup + hash).
	weight := sm.addressToSlotCount[voterAddr]
	for _, slot := range slices.Sorted(maps.Keys(sm.slotToAddress)) {
		if sm.slotToAddress[slot] == voterAddr {
			rec.VotedSlots[slot] = true
		}
	}
	if msg.VoteValid {
		rec.VotesValid += weight
	} else {
		rec.VotesInvalid += weight
	}

	// Check majority using VoteThreshold from config.
	threshold := sm.state.Config.VoteThreshold
	if rec.VotesInvalid > threshold {
		rec.Status = types.StatusInvalidated
		// Refund cost.
		sm.state.HostStats[rec.ExecutorSlot].Invalid++
		sm.state.HostStats[rec.ExecutorSlot].Cost -= rec.ActualCost
		sm.state.Balance += rec.ActualCost
	} else if rec.VotesValid > threshold {
		rec.Status = types.StatusValidated
	}

	return nil
}

func (sm *StateMachine) applyTimeout(msg *types.MsgTimeoutInference) error {
	rec, ok := sm.state.Inferences[msg.InferenceId]
	if !ok {
		return fmt.Errorf("%w: inference %d", types.ErrInferenceNotFound, msg.InferenceId)
	}

	// Validate reason matches status.
	switch msg.Reason {
	case types.TimeoutReason_TIMEOUT_REASON_REFUSED:
		if rec.Status != types.StatusPending {
			return fmt.Errorf("%w: reason=refused requires pending, got %d", types.ErrInvalidTimeoutReason, rec.Status)
		}
	case types.TimeoutReason_TIMEOUT_REASON_EXECUTION:
		if rec.Status != types.StatusStarted {
			return fmt.Errorf("%w: reason=execution requires started, got %d", types.ErrInvalidTimeoutReason, rec.Status)
		}
	default:
		return fmt.Errorf("%w: unknown reason %v", types.ErrInvalidTimeoutReason, msg.Reason)
	}

	// Count accept votes, weighted by slots per address.
	// One signature from a multi-slot validator counts for all its slots.
	acceptCount := uint32(0)
	seenAddrs := make(map[string]bool, len(msg.Votes))
	for _, vote := range msg.Votes {
		// Group membership check.
		voterAddr, ok := sm.slotToAddress[vote.VoterSlot]
		if !ok {
			return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, vote.VoterSlot)
		}

		// Duplicate voter address detection (one vote per address).
		if seenAddrs[voterAddr] {
			return fmt.Errorf("%w: slot %d", types.ErrDuplicateVote, vote.VoterSlot)
		}
		seenAddrs[voterAddr] = true

		voteContent := &types.TimeoutVoteContent{
			EscrowId:    sm.state.EscrowID,
			InferenceId: msg.InferenceId,
			Reason:      msg.Reason,
			Accept:      vote.Accept,
		}
		voteData, err := proto.Marshal(voteContent)
		if err != nil {
			return fmt.Errorf("marshal timeout vote: %w", err)
		}

		recovered, err := sm.verifier.RecoverAddress(voteData, vote.Signature)
		if err != nil {
			return fmt.Errorf("%w: vote from slot %d: %v", types.ErrInvalidVoteSig, vote.VoterSlot, err)
		}

		if recovered != voterAddr {
			return fmt.Errorf("%w: vote from slot %d: expected %s, got %s",
				types.ErrInvalidVoteSig, vote.VoterSlot, voterAddr, recovered)
		}

		if vote.Accept {
			acceptCount += sm.addressToSlotCount[voterAddr]
		}
	}

	// Check threshold using VoteThreshold from config.
	threshold := sm.state.Config.VoteThreshold
	if acceptCount <= threshold {
		return fmt.Errorf("%w: need >%d accept votes, got %d", types.ErrInsufficientVotes, threshold, acceptCount)
	}

	rec.Status = types.StatusTimedOut
	sm.state.HostStats[rec.ExecutorSlot].Missed++

	// Release reserved cost back to escrow.
	sm.state.Balance += rec.ReservedCost

	return nil
}

func (sm *StateMachine) applyRevealSeed(msg *types.MsgRevealSeed) error {
	// Guard: must be finalizing.
	if !sm.state.Finalizing {
		return types.ErrSessionNotFinalizing
	}

	// Verify slot is in group.
	revealerAddr, ok := sm.slotToAddress[msg.SlotId]
	if !ok {
		return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, msg.SlotId)
	}

	// Verify proposer signature from slot owner.
	clonedRS := proto.Clone(msg).(*types.MsgRevealSeed)
	clonedRS.ProposerSig = nil
	if err := sm.verifyProposerSig(clonedRS, msg.ProposerSig, revealerAddr); err != nil {
		return err
	}

	// Verify seed signature recovers to slot owner (proves honest derivation).
	seedAddr, err := sm.verifier.RecoverAddress([]byte(sm.state.EscrowID), msg.Signature)
	if err != nil {
		return fmt.Errorf("%w: %v", types.ErrInvalidSeedSig, err)
	}
	if seedAddr != revealerAddr {
		return fmt.Errorf("%w: expected %s, got %s", types.ErrInvalidSeedSig, revealerAddr, seedAddr)
	}

	// Dedup by address: check if any slot owned by same address already revealed.
	for _, slot := range slices.Sorted(maps.Keys(sm.state.RevealedSeeds)) {
		if sm.slotToAddress[slot] == revealerAddr {
			return fmt.Errorf("%w: address %s already revealed via slot %d", types.ErrDuplicateSeedReveal, revealerAddr, slot)
		}
	}

	// Derive seed from signature.
	seed, err := DeriveSeed(msg.Signature)
	if err != nil {
		return err
	}

	// Store seed.
	sm.state.RevealedSeeds[msg.SlotId] = seed

	// Compute compliance for this validator.
	validatorSlotCount := sm.addressToSlotCount[revealerAddr]
	executorSlotCount := uint32(0)
	requiredValidations := uint32(0)
	completedValidations := uint32(0)

	for _, infID := range slices.Sorted(maps.Keys(sm.state.Inferences)) {
		rec := sm.state.Inferences[infID]
		// Only consider finished-like statuses.
		switch rec.Status {
		case types.StatusFinished, types.StatusChallenged, types.StatusValidated, types.StatusInvalidated:
		default:
			continue
		}

		// Skip if executor is the revealer.
		executorAddr := sm.slotToAddress[rec.ExecutorSlot]
		if executorAddr == revealerAddr {
			continue
		}

		executorSlotCount = sm.addressToSlotCount[executorAddr]

		if ShouldValidate(seed, infID, validatorSlotCount, executorSlotCount, sm.totalSlots, sm.state.Config.ValidationRate) {
			requiredValidations++
			// Check if this validator actually validated this inference (bitmap check).
			// NOTE: if MsgFinishInference arrives after seed reveal, the inference is not counted in compliance.
			if rec.Status == types.StatusChallenged || rec.Status == types.StatusValidated || rec.Status == types.StatusInvalidated {
				for _, vSlot := range slices.Sorted(maps.Keys(sm.slotToAddress)) {
					if sm.slotToAddress[vSlot] == revealerAddr && (rec.ValidatedBy>>vSlot)&1 == 1 {
						completedValidations++
						break
					}
				}
			}
		}
	}

	// Write compliance to all HostStats entries owned by this address.
	for _, slot := range slices.Sorted(maps.Keys(sm.slotToAddress)) {
		if sm.slotToAddress[slot] == revealerAddr {
			if hs, ok := sm.state.HostStats[slot]; ok {
				hs.RequiredValidations = requiredValidations
				hs.CompletedValidations = completedValidations
			}
		}
	}

	return nil
}

func (sm *StateMachine) applyFinalizeRound() error {
	if sm.state.Finalizing {
		return types.ErrAlreadyFinalizing
	}
	sm.state.Finalizing = true
	return nil
}

// BuildDiffContent creates the proto DiffContent from nonce, txs, escrowID, and postStateRoot for signing.
func BuildDiffContent(escrowID string, nonce uint64, txs []*types.SubnetTx, postStateRoot []byte) *types.DiffContent {
	return &types.DiffContent{
		Nonce:         nonce,
		Txs:           txs,
		EscrowId:      escrowID,
		PostStateRoot: postStateRoot,
	}
}

// verifyProposerSig verifies that sig was produced by expectedAddress over
// msgWithoutSig (the proto message with its proposer_sig field already zeroed).
func (sm *StateMachine) verifyProposerSig(msgWithoutSig proto.Message, sig []byte, expectedAddress string) error {
	data, err := proto.Marshal(msgWithoutSig)
	if err != nil {
		return fmt.Errorf("marshal for proposer sig: %w", err)
	}

	recovered, err := sm.verifier.RecoverAddress(data, sig)
	if err != nil {
		return fmt.Errorf("%w: %v", types.ErrInvalidProposerSig, err)
	}

	if recovered != expectedAddress {
		return fmt.Errorf("%w: expected %s, got %s", types.ErrInvalidProposerSig, expectedAddress, recovered)
	}

	return nil
}

func (sm *StateMachine) TotalSlots() uint32 {
	return sm.totalSlots
}

func (sm *StateMachine) SlotAddress(slotID uint32) string {
	return sm.slotToAddress[slotID]
}

func (sm *StateMachine) AddressSlotCount(addr string) uint32 {
	return sm.addressToSlotCount[addr]
}

// IsSlotRevealed returns true if the given slot has already revealed its seed.
func (sm *StateMachine) IsSlotRevealed(slotID uint32) bool {
	_, ok := sm.state.RevealedSeeds[slotID]
	return ok
}

// GetInference returns a copy of the inference record for the given ID.
func (sm *StateMachine) GetInference(id uint64) (types.InferenceRecord, bool) {
	rec, ok := sm.state.Inferences[id]
	if !ok {
		return types.InferenceRecord{}, false
	}
	return *rec, ok
}

// RevealedSlots returns a shallow copy of the revealed seeds map.
func (sm *StateMachine) RevealedSlots() map[uint32]int64 {
	if len(sm.state.RevealedSeeds) == 0 {
		return nil
	}
	cp := make(map[uint32]int64, len(sm.state.RevealedSeeds))
	maps.Copy(cp, sm.state.RevealedSeeds)
	return cp
}

// VoteThreshold returns the session's vote threshold.
func (sm *StateMachine) VoteThreshold() uint32 {
	return sm.state.Config.VoteThreshold
}

func SortedSlotIDs(group []types.SlotAssignment) []uint32 {
	ids := make([]uint32, len(group))
	for i, s := range group {
		ids[i] = s.SlotID
	}
	slices.Sort(ids)
	return ids
}
