package state

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"subnet/internal/testutil"
	"subnet/signing"
	"subnet/types"
)

// --- Test helpers (package-specific) ---

func newTestSM(t *testing.T, hosts []*signing.Secp256k1Signer, balance uint64) (*StateMachine, *signing.Secp256k1Signer) {
	t.Helper()
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(len(hosts))
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, balance, user.Address(), verifier)
	return sm, user
}

// txStart wraps MsgStartInference in a SubnetTx.
func txStart(msg *types.MsgStartInference) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_StartInference{StartInference: msg}}
}

// txConfirm wraps MsgConfirmStart in a SubnetTx.
func txConfirm(msg *types.MsgConfirmStart) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: msg}}
}

// txFinish wraps MsgFinishInference in a SubnetTx.
func txFinish(msg *types.MsgFinishInference) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_FinishInference{FinishInference: msg}}
}

// txTimeout wraps MsgTimeoutInference in a SubnetTx.
func txTimeout(msg *types.MsgTimeoutInference) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_TimeoutInference{TimeoutInference: msg}}
}

// txValidation wraps MsgValidation in a SubnetTx.
func txValidation(msg *types.MsgValidation) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_Validation{Validation: msg}}
}

// txVote wraps MsgValidationVote in a SubnetTx.
func txVote(msg *types.MsgValidationVote) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_ValidationVote{ValidationVote: msg}}
}

// txFinalize wraps MsgFinalizeRound in a SubnetTx.
func txFinalize() *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}}
}

// --- Tests ---

func TestApplyDiff_UserSigVerification(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)
	wrongUser := testutil.MustGenerateKey(t)

	// Invalid user sig.
	diff := testutil.SignDiff(t, wrongUser, "escrow-1", 1, nil)
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidUserSig)

	// Valid user sig.
	diff = testutil.SignDiff(t, user, "escrow-1", 1, nil)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}

func TestApplyDiff_StartInference(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1,
		PromptHash:  []byte("prompt"),
		Model:       "llama",
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	rec := state.Inferences[1]
	require.NotNil(t, rec)
	require.Equal(t, types.StatusPending, rec.Status)
	require.Equal(t, uint64(150), rec.ReservedCost) // (100+50)*1
	require.Equal(t, uint64(10000-150), state.Balance)
	// Executor slot: 1 % 3 = 1
	require.Equal(t, uint32(1), rec.ExecutorSlot)
}

func TestApplyDiff_ConfirmStart(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Start inference. Executor slot: 1 % 3 = 1
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Confirm start with valid executor receipt.
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusStarted, state.Inferences[1].Status)
}

func TestApplyDiff_ConfirmStart_InvalidReceipt(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// ConfirmStart with wrong signer (host[0] instead of host[1]).
	execSig := testutil.SignExecutorReceipt(t, hosts[0], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidExecutorSig)
}

func TestApplyDiff_FinishInference(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Start + confirm.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Finish inference. Executor is slot 1 (hosts[1]).
	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 1,
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[1], finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	rec := state.Inferences[1]
	require.Equal(t, types.StatusFinished, rec.Status)
	require.Equal(t, uint64(120), rec.ActualCost) // (80+40)*1
	// Reserved was 150, actual 120 -> surplus 30 returned.
	require.Equal(t, uint64(10000-150+30), state.Balance)
	require.Equal(t, uint64(120), state.HostStats[1].Cost)
}

func TestApplyDiff_FinishInference_WrongExecutorSlot(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 2, // Wrong! Should be 1.
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrWrongExecutorSlot)
}

func TestApplyDiff_FinishInference_InvalidProposerSig(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	outsider := testutil.MustGenerateKey(t)
	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 1,
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, outsider, finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_Validation_Valid(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Validation always transitions to Challenged first (prevents sybil bypass).
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	require.Equal(t, types.StatusChallenged, st.Inferences[1].Status)
	require.Equal(t, uint32(0), st.Inferences[1].ValidatorSlot)
	require.True(t, st.Inferences[1].ValidatorValid)

	// Reach StatusValidated through vote threshold (>5/2=2 valid votes).
	var voteTxs []*types.SubnetTx
	for _, slot := range []uint32{0, 2, 3} {
		voteMsg := &types.MsgValidationVote{InferenceId: 1, VoterSlot: slot, VoteValid: true}
		voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[slot], voteMsg)
		voteTxs = append(voteTxs, txVote(voteMsg))
	}

	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, voteTxs)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st = sm.SnapshotState()
	require.Equal(t, types.StatusValidated, st.Inferences[1].Status)
}

func TestApplyDiff_Validation_SelfValidation(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 1, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[1], valMsg)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrSelfValidation)
}

func TestApplyDiff_Validation_Invalid_ChallengeVoting(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Validate (valid=false) -> challenged.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)
	require.Equal(t, types.StatusChallenged, sm.SnapshotState().Inferences[1].Status)

	// Vote invalid from 3 slots -> majority (>5/2=2) -> invalidated.
	var voteTxs []*types.SubnetTx
	for _, slot := range []uint32{0, 2, 3} {
		voteMsg := &types.MsgValidationVote{InferenceId: 1, VoterSlot: slot, VoteValid: false}
		voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[slot], voteMsg)
		voteTxs = append(voteTxs, txVote(voteMsg))
	}

	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, voteTxs)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	rec := state.Inferences[1]
	require.Equal(t, types.StatusInvalidated, rec.Status)
	require.Equal(t, uint32(1), state.HostStats[1].Invalid)
	require.Equal(t, uint64(0), state.HostStats[1].Cost)
}

func TestApplyDiff_Timeout_Refused(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusTimedOut, state.Inferences[1].Status)
	require.Equal(t, uint32(1), state.HostStats[1].Missed)
	require.Equal(t, uint64(10000), state.Balance)
}

func TestApplyDiff_Timeout_Execution(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_EXECUTION, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}

	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_EXECUTION, Votes: votes,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusTimedOut, state.Inferences[1].Status)
	require.Equal(t, uint32(1), state.HostStats[1].Missed)
	require.Equal(t, uint64(10000), state.Balance)
}

func TestApplyDiff_Timeout_WrongReason(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// reason=execution on pending -> fail.
	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_EXECUTION, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_EXECUTION, Votes: votes,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidTimeoutReason)

	// Confirm start, then reason=refused on started -> fail.
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	var votes2 []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
		v.VoterSlot = slot
		votes2 = append(votes2, v)
	}
	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes2,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidTimeoutReason)
}

func TestApplyDiff_Timeout_InsufficientVotes(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Only 2 accept votes (need >2 for 5 total slots).
	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInsufficientVotes)
}

func TestApplyDiff_Timeout_AfterFinish(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_EXECUTION, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_EXECUTION, Votes: votes,
	})})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidTimeoutReason)
}

func TestApplyDiff_Timeout_MultiSlotWeight(t *testing.T) {
	// 3 signers: signer0 owns 3 slots (0,1,2), signer1 owns 1 slot (3), signer2 owns 1 slot (4).
	// Total 5 slots. VoteThreshold = 5/2 = 2. Need >2 accept weight.
	// One vote from signer0 (slot 0) should count as weight=3.
	signers := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeMultiSlotGroup(signers, []int{3, 1, 1})
	config := testutil.DefaultConfig(len(group)) // VoteThreshold = 5/2 = 2
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	// Start inference. Executor slot = group[1%5].SlotID = 1 (owned by signer0).
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// One accept vote from signer2 (slot 4, weight=1) -- not enough alone.
	// But signer1 (slot 3, weight=1) also votes accept -> total weight=2, still not >2.
	// Need signer0 to vote (weight=3) for >2.
	vote := testutil.SignTimeoutVote(t, signers[2], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
	vote.VoterSlot = 4 // signer2's slot

	// Single vote with weight=1 should fail (need >2).
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED,
		Votes: []*types.TimeoutVote{vote},
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInsufficientVotes)

	// Now add signer0's vote (slot 0, weight=3). Total = 1+3 = 4 > 2.
	vote0 := testutil.SignTimeoutVote(t, signers[0], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
	vote0.VoterSlot = 0

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED,
		Votes: []*types.TimeoutVote{vote, vote0},
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusTimedOut, state.Inferences[1].Status)
}

func TestApplyDiff_NonceSequential(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 2, nil)
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidNonce)
}

func TestApplyDiff_MultipleMsgStartInference(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	txs := []*types.SubnetTx{
		txStart(&types.MsgStartInference{InferenceId: 1, InputLength: 10, MaxTokens: 5}),
		txStart(&types.MsgStartInference{InferenceId: 1, InputLength: 10, MaxTokens: 5}),
	}
	diff := testutil.SignDiff(t, user, "escrow-1", 1, txs)
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrMultipleStartMsgs)
}

func TestApplyDiff_FinalizeRound(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txFinalize()})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)
	require.True(t, sm.SnapshotState().Finalizing)

	// MsgStartInference after finalize -> rejected.
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 2, InputLength: 10, MaxTokens: 5,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrSessionFinalizing)

	// Second finalize -> rejected.
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txFinalize()})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrAlreadyFinalizing)
}

func TestApplyDiff_FinalizeRound_HostTxsStillAccepted(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txFinalize()})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 1,
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[1], finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 4, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
	require.Equal(t, types.StatusFinished, sm.SnapshotState().Inferences[1].Status)
}

func TestApplyDiff_DuplicateTimeout(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	var votes []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
		v.VoterSlot = slot
		votes = append(votes, v)
	}
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	var votes2 []*types.TimeoutVote
	for _, slot := range []uint32{0, 2, 3} {
		v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
		v.VoterSlot = slot
		votes2 = append(votes2, v)
	}
	diff = testutil.SignDiff(t, user, "escrow-1", 3, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes2,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidTimeoutReason)
}

func TestApplyDiff_EscrowBalanceCheck(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInsufficientBalance)
}

func TestApplyDiff_FullLifecycle(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 100000)
	nonce := uint64(0)

	outcomes := []string{
		"finished", "finished", "timed_out", "finished",
		"validated", "invalidated", "finished", "timed_out",
		"finished", "finished",
	}

	for _, outcome := range outcomes {
		// inference_id == nonce of the start diff.
		nonce++
		infID := nonce
		executorSlotIdx := infID % uint64(len(hosts))

		diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txStart(&types.MsgStartInference{
			InferenceId: infID, PromptHash: []byte("prompt"), Model: "llama",
			InputLength: 100, MaxTokens: 50, StartedAt: int64(infID) * 1000,
		})})
		_, err := sm.ApplyDiff(diff)
		require.NoError(t, err)

		if outcome == "timed_out" {
			var votes []*types.TimeoutVote
			for _, slot := range []uint32{0, 1, 2, 3, 4} {
				if slot == uint32(executorSlotIdx) {
					continue
				}
				if len(votes) >= 3 {
					break
				}
				v := testutil.SignTimeoutVote(t, hosts[slot], "escrow-1", infID, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
				v.VoterSlot = slot
				votes = append(votes, v)
			}
			nonce++
			diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
				InferenceId: infID, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: votes,
			})})
			_, err = sm.ApplyDiff(diff)
			require.NoError(t, err)
			continue
		}

		execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", infID, []byte("prompt"), "llama", 100, 50, int64(infID)*1000, int64(infID)*1000)
		nonce++
		diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
			InferenceId: infID, ExecutorSig: execSig, ConfirmedAt: int64(infID) * 1000,
		})})
		_, err = sm.ApplyDiff(diff)
		require.NoError(t, err)

		finishMsg := &types.MsgFinishInference{
			InferenceId: infID, ResponseHash: []byte("response"),
			InputTokens: 80, OutputTokens: 40, ExecutorSlot: uint32(executorSlotIdx),
		}
		finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[executorSlotIdx], finishMsg)
		nonce++
		diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txFinish(finishMsg)})
		_, err = sm.ApplyDiff(diff)
		require.NoError(t, err)

		if outcome == "finished" {
			continue
		}

		if outcome == "validated" {
			validatorSlot := uint32((executorSlotIdx + 1) % uint64(len(hosts)))
			valMsg := &types.MsgValidation{InferenceId: infID, ValidatorSlot: validatorSlot, Valid: true}
			valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[validatorSlot], valMsg)
			nonce++
			diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
			_, err = sm.ApplyDiff(diff)
			require.NoError(t, err)

			// Validation always goes to Challenged first. Reach Validated via votes.
			var voteTxs []*types.SubnetTx
			votedCount := 0
			for slot := uint32(0); slot < uint32(len(hosts)); slot++ {
				if slot == uint32(executorSlotIdx) {
					continue
				}
				if votedCount >= 3 {
					break
				}
				voteMsg := &types.MsgValidationVote{InferenceId: infID, VoterSlot: slot, VoteValid: true}
				voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[slot], voteMsg)
				voteTxs = append(voteTxs, txVote(voteMsg))
				votedCount++
			}
			nonce++
			diff = testutil.SignDiff(t, user, "escrow-1", nonce, voteTxs)
			_, err = sm.ApplyDiff(diff)
			require.NoError(t, err)
			continue
		}

		if outcome == "invalidated" {
			validatorSlot := uint32((executorSlotIdx + 1) % uint64(len(hosts)))
			valMsg := &types.MsgValidation{InferenceId: infID, ValidatorSlot: validatorSlot, Valid: false}
			valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[validatorSlot], valMsg)
			nonce++
			diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
			_, err = sm.ApplyDiff(diff)
			require.NoError(t, err)

			var voteTxs []*types.SubnetTx
			votedCount := 0
			for slot := uint32(0); slot < uint32(len(hosts)); slot++ {
				if slot == uint32(executorSlotIdx) || slot == validatorSlot {
					continue
				}
				if votedCount >= 3 {
					break
				}
				voteMsg := &types.MsgValidationVote{InferenceId: infID, VoterSlot: slot, VoteValid: false}
				voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[slot], voteMsg)
				voteTxs = append(voteTxs, txVote(voteMsg))
				votedCount++
			}
			nonce++
			diff = testutil.SignDiff(t, user, "escrow-1", nonce, voteTxs)
			_, err = sm.ApplyDiff(diff)
			require.NoError(t, err)
		}
	}

	state := sm.SnapshotState()
	var finished, timedOut, validated, invalidated int
	for _, rec := range state.Inferences {
		switch rec.Status {
		case types.StatusFinished:
			finished++
		case types.StatusTimedOut:
			timedOut++
		case types.StatusValidated:
			validated++
		case types.StatusInvalidated:
			invalidated++
		}
	}
	require.Equal(t, 6, finished)
	require.Equal(t, 2, timedOut)
	require.Equal(t, 1, validated)
	require.Equal(t, 1, invalidated)

	totalCost := uint64(0)
	for _, hs := range state.HostStats {
		totalCost += hs.Cost
	}
	require.Equal(t, uint64(100000)-totalCost, state.Balance)
}

func TestApplyDiff_InferenceIDMustMatchNonce(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// inference_id=42 at nonce=1 -> rejected.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 42, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidInferenceID)
}

// --- 4 new tests ---

func TestApplyDiff_DuplicateInferenceID(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// First start succeeds.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Second start with same ID rejected (inference_id=1 != nonce=2).
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt2"), Model: "llama",
		InputLength: 50, MaxTokens: 25, StartedAt: 2000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidInferenceID)
}

func TestApplyDiff_Timeout_DuplicateVoterSlot(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Slot 0 votes twice.
	v0a := testutil.SignTimeoutVote(t, hosts[0], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
	v0a.VoterSlot = 0
	v0b := testutil.SignTimeoutVote(t, hosts[0], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
	v0b.VoterSlot = 0
	v2 := testutil.SignTimeoutVote(t, hosts[2], "escrow-1", 1, types.TimeoutReason_TIMEOUT_REASON_REFUSED, true)
	v2.VoterSlot = 2

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txTimeout(&types.MsgTimeoutInference{
		InferenceId: 1, Reason: types.TimeoutReason_TIMEOUT_REASON_REFUSED, Votes: []*types.TimeoutVote{v0a, v0b, v2},
	})})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrDuplicateVote)
}

func TestApplyDiff_ValidationVote_AlreadyResolved(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Challenge.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)
	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// 4 votes batched in one diff. 3 are enough to resolve; 4th should silently succeed.
	var voteTxs []*types.SubnetTx
	for _, slot := range []uint32{0, 2, 3, 4} {
		voteMsg := &types.MsgValidationVote{InferenceId: 1, VoterSlot: slot, VoteValid: false}
		voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[slot], voteMsg)
		voteTxs = append(voteTxs, txVote(voteMsg))
	}

	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, voteTxs)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusInvalidated, state.Inferences[1].Status)
}

func TestSnapshotState_DeepCopy(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Start an inference to populate state.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Get state and mutate the copy.
	stateCopy := sm.SnapshotState()
	stateCopy.Balance = 999999
	stateCopy.Inferences[1].Status = types.StatusTimedOut
	stateCopy.Inferences[1].PromptHash[0] = 0xFF
	stateCopy.HostStats[0].Cost = 999
	stateCopy.Group[0].Weight = 999

	// Verify original state is unaffected.
	original := sm.SnapshotState()
	require.Equal(t, uint64(10000-150), original.Balance)
	require.Equal(t, types.StatusPending, original.Inferences[1].Status)
	require.Equal(t, byte('p'), original.Inferences[1].PromptHash[0])
	require.Equal(t, uint64(0), original.HostStats[0].Cost)
	require.Equal(t, uint64(1), original.Group[0].Weight)
}

// --- Helper for common start + confirm + finish flow ---

func applyStartConfirmFinish(t *testing.T, sm *StateMachine, user *signing.Secp256k1Signer, hosts []*signing.Secp256k1Signer, inferenceID uint64) {
	t.Helper()
	executorSlotIdx := inferenceID % uint64(len(hosts))
	nonce := sm.SnapshotState().LatestNonce + 1

	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: inferenceID, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", inferenceID, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: inferenceID, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	finishMsg := &types.MsgFinishInference{
		InferenceId: inferenceID, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: uint32(executorSlotIdx),
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[executorSlotIdx], finishMsg)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}

// txRevealSeed wraps MsgRevealSeed in a SubnetTx.
func txRevealSeed(msg *types.MsgRevealSeed) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_RevealSeed{RevealSeed: msg}}
}

// --- Wrong-proposer tests ---

func TestApplyDiff_FinishInference_WrongProposer(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Start + confirm. Executor for inference 1 is slot 1 (hosts[1]).
	applyStartConfirmFinish_Setup(t, sm, user, hosts, 1)

	// Sign finish with hosts[0] (in group, but not the executor).
	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 1,
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], finishMsg)

	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txFinish(finishMsg)})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_Validation_WrongProposer(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Validator is slot 0, but sign with hosts[2] (in group, wrong slot).
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[2], valMsg)

	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_ValidationVote_WrongProposer(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Challenge.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Vote from slot 2, but sign with hosts[3] (in group, wrong slot).
	voteMsg := &types.MsgValidationVote{InferenceId: 1, VoterSlot: 2, VoteValid: false}
	voteMsg.ProposerSig = testutil.SignProposerTx(t, hosts[3], voteMsg)

	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txVote(voteMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_RevealSeed_WrongProposer(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Finalize first.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// RevealSeed from slot 0, signed by hosts[1] (in group, wrong slot).
	seedSig, _ := hosts[0].Sign([]byte("escrow-1"))
	seedMsg := &types.MsgRevealSeed{SlotId: 0, Signature: seedSig}
	seedMsg.ProposerSig = testutil.SignProposerTx(t, hosts[1], seedMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_RevealSeed_InvalidSlot(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Finalize first.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// RevealSeed with slot 99 (not in group).
	seedMsg := &types.MsgRevealSeed{SlotId: 99, Signature: []byte("seed")}
	seedMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], seedMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrSlotNotInGroup)
}

func TestApplyDiff_CostOverflow_StartInference(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, math.MaxUint64)

	// InputLength + MaxTokens overflows uint64.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: math.MaxUint64, MaxTokens: 1, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrCostOverflow)

	// Multiplication overflows: large input * price.
	diff = testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: math.MaxUint64 / 2, MaxTokens: 1, StartedAt: 1000,
	})})
	// With TokenPrice=1, the mul won't overflow. Use a custom SM with higher price.
	config := types.SessionConfig{TokenPrice: 3, VoteThreshold: 1}
	group := testutil.MakeGroup(hosts)
	verifier := signing.NewSecp256k1Verifier()
	smHigh := NewStateMachine("escrow-1", config, group, math.MaxUint64, user.Address(), verifier)

	diff = testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: math.MaxUint64 / 2, MaxTokens: 1, StartedAt: 1000,
	})})
	_, err = smHigh.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrCostOverflow)
}

func TestApplyDiff_AtomicRollback(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// First: apply a valid start.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	balanceBefore := sm.SnapshotState().Balance

	// Diff with two txs: a valid confirm, then an invalid finish (wrong executor slot).
	// The confirm would succeed, modifying state, but the finish fails.
	// With atomic rollback, the state should be unchanged.
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000, 1000)

	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 2, // Wrong executor slot.
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[2], finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{
		txConfirm(&types.MsgConfirmStart{InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000}),
		txFinish(finishMsg),
	})
	_, err = sm.ApplyDiff(diff)
	require.Error(t, err)

	// State should be unchanged (atomic rollback).
	st := sm.SnapshotState()
	require.Equal(t, balanceBefore, st.Balance)
	require.Equal(t, types.StatusPending, st.Inferences[1].Status, "should still be pending after rollback")
	require.Equal(t, uint64(1), st.LatestNonce, "nonce should not advance on failure")
}

// --- Attack / bug regression tests ---

func TestAttack_SybilValidationBypass(t *testing.T) {
	// Attack: attacker with 2 slots executes on slot A, submits MsgValidation(Valid=true)
	// from slot B. Without the fix, inference instantly becomes StatusValidated (terminal).
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Slot 0 submits MsgValidation(Valid=true). Must NOT become StatusValidated.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	require.Equal(t, types.StatusChallenged, st.Inferences[1].Status,
		"validation must always go to Challenged, not directly to Validated")
}

func TestApplyDiff_Validation_MultipleValidators(t *testing.T) {
	// Second MsgValidation for the same inference records in bitmap.
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// First validation -> Challenged.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)
	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)
	rec := sm.SnapshotState().Inferences[1]
	require.Equal(t, types.StatusChallenged, rec.Status)
	var expectedBitmap1 types.Bitmap128
	expectedBitmap1.Set(0)
	require.Equal(t, expectedBitmap1, rec.ValidatedBy, "first validator bit must be set")

	// Second validation from different host -> bitmap updated.
	valMsg2 := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: true}
	valMsg2.ProposerSig = testutil.SignProposerTx(t, hosts[2], valMsg2)
	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg2)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	rec = sm.SnapshotState().Inferences[1]
	var expectedBitmap2 types.Bitmap128
	expectedBitmap2.Set(0)
	expectedBitmap2.Set(2)
	require.Equal(t, expectedBitmap2, rec.ValidatedBy, "both validator bits must be set")
}

func TestApplyDiff_Validation_DuplicateAddress(t *testing.T) {
	// Multi-slot validator tries to validate twice via different slots -> ErrDuplicateValidation.
	signers := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeMultiSlotGroup(signers, []int{2, 1, 1})
	config := testutil.DefaultConfig(len(group))
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	// Inference 1: executor = group[1%4].SlotID = 1 (owned by signer[0]).
	applyStartConfirmFinishMultiSlot(t, sm, user, signers, group, 1)

	// First validation from signer[1] (slot 2) -> Challenged.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, signers[1], valMsg)
	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Same address (signer[1]) tries again from slot 2 -> ErrDuplicateValidation.
	valMsg2 := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: false}
	valMsg2.ProposerSig = testutil.SignProposerTx(t, signers[1], valMsg2)
	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg2)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrDuplicateValidation)
}

func TestApplyDiff_ValidationVote_MultiSlotWeight(t *testing.T) {
	// 3 signers: signer[0] owns 2 slots (0,1), signer[1] owns 1 slot (2), signer[2] owns 1 slot (3).
	// Total 4 slots. VoteThreshold = 4/2 = 2. Need >2 weighted votes to resolve.
	signers := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeMultiSlotGroup(signers, []int{2, 1, 1})
	config := testutil.DefaultConfig(len(group)) // VoteThreshold = 4/2 = 2
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	// Inference 1: executor = group[1%4].SlotID = 1 (owned by signer[0]).
	applyStartConfirmFinishMultiSlot(t, sm, user, signers, group, 1)

	// Challenge.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, signers[1], valMsg)
	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Signer[0] votes invalid from slot 0. Weight = 2 (owns slots 0 and 1).
	voteMsg := &types.MsgValidationVote{InferenceId: 1, VoterSlot: 0, VoteValid: false}
	voteMsg.ProposerSig = testutil.SignProposerTx(t, signers[0], voteMsg)
	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txVote(voteMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	require.Equal(t, uint32(2), st.Inferences[1].VotesInvalid, "multi-slot vote should have weight 2")
}

func TestApplyDiff_ValidationVote_MultiSlotDedup(t *testing.T) {
	// Same signer voting from two different owned slots must be rejected as duplicate.
	signers := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeMultiSlotGroup(signers, []int{2, 1, 1})
	config := testutil.DefaultConfig(len(group))
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	applyStartConfirmFinishMultiSlot(t, sm, user, signers, group, 1)

	// Challenge.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: false}
	valMsg.ProposerSig = testutil.SignProposerTx(t, signers[1], valMsg)
	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Signer[0] votes from slot 0.
	vote1 := &types.MsgValidationVote{InferenceId: 1, VoterSlot: 0, VoteValid: false}
	vote1.ProposerSig = testutil.SignProposerTx(t, signers[0], vote1)

	// Signer[0] votes again from slot 1 (other owned slot) -> must be rejected.
	vote2 := &types.MsgValidationVote{InferenceId: 1, VoterSlot: 1, VoteValid: false}
	vote2.ProposerSig = testutil.SignProposerTx(t, signers[0], vote2)

	nonce = sm.SnapshotState().LatestNonce + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txVote(vote1), txVote(vote2)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrDuplicateVote)
}

// applyStartConfirmFinishMultiSlot works with multi-slot groups.
func applyStartConfirmFinishMultiSlot(t *testing.T, sm *StateMachine, user *signing.Secp256k1Signer, signers []*signing.Secp256k1Signer, group []types.SlotAssignment, inferenceID uint64) {
	t.Helper()
	executorSlotIdx := inferenceID % uint64(len(group))
	executorSlot := group[executorSlotIdx]

	// Find the signer that owns the executor slot.
	var executorSigner *signing.Secp256k1Signer
	for _, s := range signers {
		if s.Address() == executorSlot.ValidatorAddress {
			executorSigner = s
			break
		}
	}
	require.NotNil(t, executorSigner)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: inferenceID, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, executorSigner, "escrow-1", inferenceID, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: inferenceID, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	finishMsg := &types.MsgFinishInference{
		InferenceId: inferenceID, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: executorSlot.SlotID,
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, executorSigner, finishMsg)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txFinish(finishMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}

func TestApplyDiff_PostStateRoot_Valid(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Use a second SM to compute the correct post_state_root.
	verifier := signing.NewSecp256k1Verifier()
	group := testutil.MakeGroup(hosts)
	config := testutil.DefaultConfig(len(hosts))
	sm2 := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	txs := []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})}

	// Compute root from the replica.
	root, err := sm2.ApplyLocal(1, txs)
	require.NoError(t, err)

	// Sign diff with correct post_state_root.
	diff := testutil.SignDiffWithRoot(t, user, "escrow-1", 1, txs, root)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}

func TestApplyDiff_PostStateRoot_Mismatch(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	txs := []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: 1, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})}

	// Sign diff with wrong post_state_root.
	diff := testutil.SignDiffWithRoot(t, user, "escrow-1", 1, txs, []byte("wrong-root"))
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrPostStateRootMismatch)
}

func TestApplyDiff_PostStateRoot_Empty_Accepted(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// Diff without post_state_root (backwards-compatible).
	diff := testutil.SignDiff(t, user, "escrow-1", 1, nil)
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)
}

// --- Phase 4: RevealSeed with state effects ---

func TestApplyDiff_RevealSeed_Success(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	// Create a finished inference (executor=slot 1).
	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Finalize.
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 0 (not the executor).
	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	require.Contains(t, st.RevealedSeeds, uint32(0))
	require.NotZero(t, st.RevealedSeeds[0])
}

func TestApplyDiff_RevealSeed_DuplicateAddress(t *testing.T) {
	// Multi-slot: signer[0] owns slots 0,1. Revealing from slot 1 after slot 0 should fail.
	signers := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeMultiSlotGroup(signers, []int{2, 1, 1})
	config := testutil.DefaultConfig(len(group))
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 10000, user.Address(), verifier)

	// Finalize.
	nonce := uint64(1)
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// First reveal from slot 0 (signer[0]).
	seedMsg := testutil.SignRevealSeed(t, signers[0], "escrow-1", 0)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Second reveal from slot 1 (same signer[0]) -> duplicate.
	seedMsg2 := testutil.SignRevealSeed(t, signers[0], "escrow-1", 1)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg2)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrDuplicateSeedReveal)
}

func TestApplyDiff_RevealSeed_NotFinalizing(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrSessionNotFinalizing)
}

// Fixed private keys for reproducible seed derivation.
// signer[0] signing escrowID "escrow-1" produces seed=8507102209880137399.
// With 3 hosts, 100% rate, prob=0.5: ShouldValidate(seed, 1)=true, (seed, 4)=false, (seed, 7)=false.
var fixedKeys = []string{
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
}

func fixedSigners(t *testing.T) []*signing.Secp256k1Signer {
	t.Helper()
	out := make([]*signing.Secp256k1Signer, len(fixedKeys))
	for i, k := range fixedKeys {
		out[i] = testutil.MustSignerFromHex(t, k)
	}
	return out
}

func TestApplyDiff_RevealSeed_ComplianceComputed(t *testing.T) {
	hosts := fixedSigners(t)
	user := testutil.MustSignerFromHex(t, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(len(hosts)) / 2,
		ValidationRate:   10000, // 100%
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	// 3 finished inferences. Each uses 3 nonces (start, confirm, finish).
	// inference 1: executor = slot 1%3=1. inference 4: slot 4%3=1. inference 7: slot 7%3=1.
	// All executors are slot 1 (hosts[1]), so slot 0 is eligible for all 3.
	applyStartConfirmFinish(t, sm, user, hosts, 1)
	applyStartConfirmFinish(t, sm, user, hosts, 4)
	applyStartConfirmFinish(t, sm, user, hosts, 7)

	// Finalize.
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 0 (hosts[0]).
	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	hs := st.HostStats[0]

	// With fixed key, seed=8507102209880137399.
	// prob = 1.0 * 1/(3-1) = 0.5.
	// ShouldValidate(seed, 1) = true (0.433 < 0.5)
	// ShouldValidate(seed, 4) = false (0.570 >= 0.5)
	// ShouldValidate(seed, 7) = false (0.821 >= 0.5)
	// So RequiredValidations = 1, CompletedValidations = 0 (no actual validation).
	require.Equal(t, uint32(1), hs.RequiredValidations,
		"exactly 1 of 3 inferences should require validation with this seed")
	require.Equal(t, uint32(0), hs.CompletedValidations,
		"no validations were performed")
}

func TestApplyDiff_RevealSeed_CompletedValidationsCounted(t *testing.T) {
	hosts := fixedSigners(t)
	user := testutil.MustSignerFromHex(t, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(len(hosts)) / 2,
		ValidationRate:   10000,
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	// Inference 1: executor = slot 1%3=1 (hosts[1]).
	// ShouldValidate(seed, 1) = true with this fixed key. So it counts as required.
	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// hosts[0] validates inference 1: Finished -> Challenged.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Finalize.
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 0 (hosts[0], the validator).
	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	hs := st.HostStats[0]

	// ShouldValidate(seed, 1) = true for this key.
	// hosts[0] validated inference 1 (ValidatorSlot=0, status=Challenged).
	require.Equal(t, uint32(1), hs.RequiredValidations,
		"inference 1 should require validation")
	require.Equal(t, uint32(1), hs.CompletedValidations,
		"hosts[0] validated inference 1, so CompletedValidations must be 1")
}

func TestApplyDiff_Validation_MultipleValidators_ComplianceCredit(t *testing.T) {
	// Two validators validate the same inference. Both reveal seeds. Both get compliance credit.
	hosts := fixedSigners(t)
	user := testutil.MustSignerFromHex(t, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(len(hosts)) / 2,
		ValidationRate:   10000, // 100%
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	// Inference 1: executor = slot 1%3=1 (hosts[1]).
	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// hosts[0] validates inference 1: Finished -> Challenged.
	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// hosts[2] also validates inference 1: records in bitmap.
	valMsg2 := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 2, Valid: true}
	valMsg2.ProposerSig = testutil.SignProposerTx(t, hosts[2], valMsg2)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg2)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Finalize.
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txFinalize()})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 0 (hosts[0]).
	seedMsg0 := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg0)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 2 (hosts[2]).
	seedMsg2 := testutil.SignRevealSeed(t, hosts[2], "escrow-1", 2)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg2)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()

	// hosts[0] should have CompletedValidations >= 1 if ShouldValidate returns true.
	hs0 := st.HostStats[0]
	if hs0.RequiredValidations > 0 {
		require.Equal(t, uint32(1), hs0.CompletedValidations,
			"hosts[0] validated inference 1 via bitmap, should get credit")
	}

	// hosts[2] should also have CompletedValidations >= 1 if ShouldValidate returns true.
	hs2 := st.HostStats[2]
	if hs2.RequiredValidations > 0 {
		require.Equal(t, uint32(1), hs2.CompletedValidations,
			"hosts[2] validated inference 1 via bitmap, should get credit")
	}
}

func TestApplyDiff_RevealSeed_ZeroRateNoRequirements(t *testing.T) {
	hosts := fixedSigners(t)
	user := testutil.MustSignerFromHex(t, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(len(hosts)) / 2,
		ValidationRate:   0, // 0% -- no validations required
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	// Finalize + reveal.
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	require.Equal(t, uint32(0), st.HostStats[0].RequiredValidations,
		"0%% validation rate must produce zero required validations")
	require.Equal(t, uint32(0), st.HostStats[0].CompletedValidations)
}

func TestApplyDiff_RevealSeed_MultipleRevealers(t *testing.T) {
	hosts := fixedSigners(t)
	user := testutil.MustSignerFromHex(t, "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout: 60, ExecutionTimeout: 1200, TokenPrice: 1,
		VoteThreshold: uint32(len(hosts)) / 2, ValidationRate: 10000,
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	// 3 inferences, all with executor=slot 1 (hosts[1]).
	applyStartConfirmFinish(t, sm, user, hosts, 1)
	applyStartConfirmFinish(t, sm, user, hosts, 4)
	applyStartConfirmFinish(t, sm, user, hosts, 7)

	// Finalize.
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from all 3 validators.
	for i := 0; i < 3; i++ {
		seedMsg := testutil.SignRevealSeed(t, hosts[i], "escrow-1", uint32(i))
		nonce = sm.LatestNonce() + 1
		diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
		_, err = sm.ApplyDiff(diff)
		require.NoError(t, err)
	}

	st := sm.SnapshotState()

	// Each signer derives a different seed from signing escrowID.
	// signer[0] seed=8507102209880137399 -> ShouldValidate: inf1=true, inf4=false, inf7=false -> Required=1
	// signer[1] seed=8250581583015032772 -> executor for all 3 inferences -> Required=0
	// signer[2] seed=88554756047201157  -> ShouldValidate: inf1=false, inf4=false, inf7=true -> Required=1
	require.NotEqual(t, st.RevealedSeeds[0], st.RevealedSeeds[1], "different keys must produce different seeds")
	require.NotEqual(t, st.RevealedSeeds[1], st.RevealedSeeds[2], "different keys must produce different seeds")

	require.Equal(t, uint32(1), st.HostStats[0].RequiredValidations,
		"signer[0]: only inference 1 passes ShouldValidate")
	require.Equal(t, uint32(0), st.HostStats[1].RequiredValidations,
		"signer[1]: executor of all inferences, nothing to validate")
	require.Equal(t, uint32(1), st.HostStats[2].RequiredValidations,
		"signer[2]: only inference 7 passes ShouldValidate")

	// No actual validations were performed.
	for i := uint32(0); i < 3; i++ {
		require.Equal(t, uint32(0), st.HostStats[i].CompletedValidations)
	}
}

func TestApplyDiff_RevealSeed_ExecutorSkipsSelf(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	user := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup(hosts)
	config := types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(len(hosts)) / 2,
		ValidationRate:   10000,
	}
	verifier := signing.NewSecp256k1Verifier()
	sm := NewStateMachine("escrow-1", config, group, 100000, user.Address(), verifier)

	// Inference 1: nonces 1,2,3. executor = slot 1%3 = 1 = hosts[1].
	// But we want hosts[0] to be the executor. Inference ID 3 has executor slot 3%3=0.
	// applyStartConfirmFinish uses nonce = latestNonce+1 = 1 for the start,
	// so inference_id must be 1. executor = slot 1%3 = 1 (hosts[1]).
	// Let's use the fact that inference_id=nonce. We need inference where executor=slot 0.
	// slot 0 executes when inference_id % 3 == 0. E.g. inference_id = 3.
	// But applyStartConfirmFinish starts at nonce=1, so inference_id must be 1.
	// Instead, advance nonce first, then use inference_id = 3 at nonce = 3.
	// Simplest: apply two empty diffs first.
	diff := testutil.SignDiff(t, user, "escrow-1", 1, nil)
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, nil)
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Now nonce=2, next will be 3. inference_id=3, executor = 3%3=0 = hosts[0].
	applyStartConfirmFinish(t, sm, user, hosts, 3)

	// Finalize. nonce is now 5, next is 6.
	nonce := sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal from slot 0 (hosts[0], who is the executor of inference 3).
	seedMsg := testutil.SignRevealSeed(t, hosts[0], "escrow-1", 0)
	nonce = sm.LatestNonce() + 1
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)

	st := sm.SnapshotState()
	// Executor should skip its own inference, so RequiredValidations = 0.
	require.Equal(t, uint32(0), st.HostStats[0].RequiredValidations,
		"executor should not be required to validate its own inferences")
}

func TestApplyDiff_RevealSeed_ForgeSeedSig(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 10000)

	// Finalize.
	nonce := uint64(1)
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Create a MsgRevealSeed where the seed signature is from a different key.
	wrongKey := testutil.MustGenerateKey(t)
	wrongSeedSig, err := wrongKey.Sign([]byte("escrow-1"))
	require.NoError(t, err)

	seedMsg := &types.MsgRevealSeed{
		SlotId:    0,
		Signature: wrongSeedSig,
	}
	// Proposer sig is correct (from hosts[0]).
	cloned := &types.MsgRevealSeed{SlotId: 0, Signature: wrongSeedSig}
	seedMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], cloned)

	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err = sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidSeedSig)
}

// applyStartConfirmFinish_Setup applies start + confirm only (no finish).
// Used when we need to test finish with specific proposer.
func applyStartConfirmFinish_Setup(t *testing.T, sm *StateMachine, user *signing.Secp256k1Signer, hosts []*signing.Secp256k1Signer, inferenceID uint64) {
	t.Helper()
	executorSlotIdx := inferenceID % uint64(len(hosts))
	nonce := sm.LatestNonce() + 1

	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txStart(&types.MsgStartInference{
		InferenceId: inferenceID, PromptHash: []byte("prompt"), Model: "llama",
		InputLength: 100, MaxTokens: 50, StartedAt: 1000,
	})})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", inferenceID, []byte("prompt"), "llama", 100, 50, 1000, 1000)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: inferenceID, ExecutorSig: execSig, ConfirmedAt: 1000,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}

func TestPenalizeUnrevealedSeeds(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{
		testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t),
	}
	sm, user := newTestSM(t, hosts, 100000)

	// 3 finished inferences (each uses 3 nonces).
	applyStartConfirmFinish(t, sm, user, hosts, 1)
	applyStartConfirmFinish(t, sm, user, hosts, 4)
	applyStartConfirmFinish(t, sm, user, hosts, 7)

	// Finalize.
	nonce := sm.LatestNonce() + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{
		{Tx: &types.SubnetTx_FinalizeRound{FinalizeRound: &types.MsgFinalizeRound{}}},
	})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	// Reveal seeds for hosts 0 and 1 only.
	for _, slot := range []uint32{0, 1} {
		seedMsg := testutil.SignRevealSeed(t, hosts[slot], "escrow-1", slot)
		nonce = sm.LatestNonce() + 1
		diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txRevealSeed(seedMsg)})
		_, err = sm.ApplyDiff(diff)
		require.NoError(t, err)
	}

	// Snapshot before penalty to check hosts 0 and 1 are not changed.
	prePenalty := sm.SnapshotState()
	hs0Before := *prePenalty.HostStats[0]
	hs1Before := *prePenalty.HostStats[1]

	// Host 2 has no seed revealed -- RequiredValidations should be 0 before penalty.
	require.Equal(t, uint32(0), prePenalty.HostStats[2].RequiredValidations)

	sm.PenalizeUnrevealedSeeds()

	st := sm.SnapshotState()

	// Host 2: penalized. ValidationRate=5000 (50%), 3 inferences -> required = 3*5000/10000 = 1.
	require.Equal(t, uint32(1), st.HostStats[2].RequiredValidations,
		"unrevealed host should have RequiredValidations = numInf * rate / 10000")
	require.Equal(t, uint32(0), st.HostStats[2].CompletedValidations,
		"unrevealed host should have CompletedValidations = 0")

	// Hosts 0 and 1: unchanged by penalty.
	require.Equal(t, hs0Before.RequiredValidations, st.HostStats[0].RequiredValidations,
		"revealed host 0 RequiredValidations should be unchanged")
	require.Equal(t, hs0Before.CompletedValidations, st.HostStats[0].CompletedValidations,
		"revealed host 0 CompletedValidations should be unchanged")
	require.Equal(t, hs1Before.RequiredValidations, st.HostStats[1].RequiredValidations,
		"revealed host 1 RequiredValidations should be unchanged")
	require.Equal(t, hs1Before.CompletedValidations, st.HostStats[1].CompletedValidations,
		"revealed host 1 CompletedValidations should be unchanged")
}
