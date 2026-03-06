package state

import (
	"fmt"
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
			EscrowID:   escrowID,
			Config:     config,
			Group:      groupCopy,
			Balance:    balance,
			Inferences: make(map[uint64]*types.InferenceRecord),
			HostStats:  hostStats,
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

// ApplyDiff validates and applies a diff, returning the state root.
func (sm *StateMachine) ApplyDiff(diff types.Diff) ([]byte, error) {
	// 1. Verify user signature.
	diffContent := BuildDiffContent(sm.state.EscrowID, diff.Nonce, diff.Txs)
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

	// 2. Validate nonce.
	expectedNonce := sm.state.LatestNonce + 1
	if diff.Nonce != expectedNonce {
		return nil, fmt.Errorf("%w: expected %d, got %d", types.ErrInvalidNonce, expectedNonce, diff.Nonce)
	}

	// 3. Validate at most one MsgStartInference per diff, and inference_id == nonce.
	startCount := 0
	for _, tx := range diff.Txs {
		if start := tx.GetStartInference(); start != nil {
			startCount++
			if start.InferenceId != diff.Nonce {
				return nil, types.ErrInvalidInferenceID
			}
		}
	}
	if startCount > 1 {
		return nil, types.ErrMultipleStartMsgs
	}

	// 4. Snapshot mutable state for rollback on error.
	snap := sm.snapshotMutable()

	// 5. Apply each tx.
	for _, tx := range diff.Txs {
		if err := sm.applyTx(tx); err != nil {
			sm.restoreMutable(snap)
			return nil, err
		}
	}

	// 6. Update nonce.
	sm.state.LatestNonce = diff.Nonce

	// 7. Compute state root.
	// TODO: optimize for sure
	root, err := ComputeStateRoot(sm.state.Balance, sm.state.HostStats, sm.state.Inferences)
	if err != nil {
		return nil, fmt.Errorf("compute state root: %w", err)
	}

	logging.Debug("applied diff", "subsystem", "state", "nonce", diff.Nonce, "txs", len(diff.Txs))
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
			for sk, sv := range v.VotedSlots {
				cp.VotedSlots[sk] = sv
			}
		}
		s.Inferences[k] = &cp
	}

	return s
}

// mutableSnapshot holds the mutable fields of EscrowState for rollback.
type mutableSnapshot struct {
	Balance    uint64
	Finalizing bool
	Inferences map[uint64]*types.InferenceRecord
	HostStats  map[uint32]*types.HostStats
}

func (sm *StateMachine) snapshotMutable() mutableSnapshot {
	infCopy := make(map[uint64]*types.InferenceRecord, len(sm.state.Inferences))
	for k, v := range sm.state.Inferences {
		cp := *v
		if v.VotedSlots != nil {
			cp.VotedSlots = make(map[uint32]bool, len(v.VotedSlots))
			for sk, sv := range v.VotedSlots {
				cp.VotedSlots[sk] = sv
			}
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

	return mutableSnapshot{
		Balance:    sm.state.Balance,
		Finalizing: sm.state.Finalizing,
		Inferences: infCopy,
		HostStats:  hsCopy,
	}
}

func (sm *StateMachine) restoreMutable(snap mutableSnapshot) {
	sm.state.Balance = snap.Balance
	sm.state.Finalizing = snap.Finalizing
	sm.state.Inferences = snap.Inferences
	sm.state.HostStats = snap.HostStats
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

	// No-op for already-resolved inferences (prevents mempool stall on redundant validations).
	if rec.Status == types.StatusChallenged || rec.Status == types.StatusValidated || rec.Status == types.StatusInvalidated {
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
	for slot, voted := range rec.VotedSlots {
		if voted && sm.slotToAddress[slot] == voterAddr {
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
	for slot, addr := range sm.slotToAddress {
		if addr == voterAddr {
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
	// Verify slot is in group.
	if _, ok := sm.slotToAddress[msg.SlotId]; !ok {
		return fmt.Errorf("%w: slot %d", types.ErrSlotNotInGroup, msg.SlotId)
	}

	// Verify proposer signature from slot owner.
	clonedRS := proto.Clone(msg).(*types.MsgRevealSeed)
	clonedRS.ProposerSig = nil
	if err := sm.verifyProposerSig(clonedRS, msg.ProposerSig, sm.slotToAddress[msg.SlotId]); err != nil {
		return err
	}

	// Accept but no state effect in Phase 1 (ShouldValidate deferred to Phase 4).
	return nil
}

func (sm *StateMachine) applyFinalizeRound() error {
	if sm.state.Finalizing {
		return types.ErrAlreadyFinalizing
	}
	sm.state.Finalizing = true
	return nil
}

// BuildDiffContent creates the proto DiffContent from nonce, txs, and escrowID for signing.
// Since Diff.Txs is already []*SubnetTx, no conversion is needed.
func BuildDiffContent(escrowID string, nonce uint64, txs []*types.SubnetTx) *types.DiffContent {
	return &types.DiffContent{
		Nonce:    nonce,
		Txs:      txs,
		EscrowId: escrowID,
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

func SortedSlotIDs(group []types.SlotAssignment) []uint32 {
	ids := make([]uint32, len(group))
	for i, s := range group {
		ids[i] = s.SlotID
	}
	slices.Sort(ids)
	return ids
}
