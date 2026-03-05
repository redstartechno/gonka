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
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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
	execSig := testutil.SignExecutorReceipt(t, hosts[0], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	applyStartConfirmFinish(t, sm, user, hosts, 1)

	valMsg := &types.MsgValidation{InferenceId: 1, ValidatorSlot: 0, Valid: true}
	valMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], valMsg)

	nonce := sm.SnapshotState().LatestNonce + 1
	diff := testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txValidation(valMsg)})
	_, err := sm.ApplyDiff(diff)
	require.NoError(t, err)

	state := sm.SnapshotState()
	require.Equal(t, types.StatusValidated, state.Inferences[1].Status)
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

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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

	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)
	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig,
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

		execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", infID, []byte("prompt"), "llama", 100, 50, int64(infID)*1000)
		nonce++
		diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
			InferenceId: infID, ExecutorSig: execSig,
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

	execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", inferenceID, []byte("prompt"), "llama", 100, 50, 1000)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: inferenceID, ExecutorSig: execSig,
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

	// RevealSeed from slot 0, signed by hosts[1] (in group, wrong slot).
	seedMsg := &types.MsgRevealSeed{SlotId: 0, Signature: []byte("seed")}
	seedMsg.ProposerSig = testutil.SignProposerTx(t, hosts[1], seedMsg)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err := sm.ApplyDiff(diff)
	require.ErrorIs(t, err, types.ErrInvalidProposerSig)
}

func TestApplyDiff_RevealSeed_InvalidSlot(t *testing.T) {
	hosts := []*signing.Secp256k1Signer{testutil.MustGenerateKey(t), testutil.MustGenerateKey(t), testutil.MustGenerateKey(t)}
	sm, user := newTestSM(t, hosts, 10000)

	// RevealSeed with slot 99 (not in group).
	seedMsg := &types.MsgRevealSeed{SlotId: 99, Signature: []byte("seed")}
	seedMsg.ProposerSig = testutil.SignProposerTx(t, hosts[0], seedMsg)

	diff := testutil.SignDiff(t, user, "escrow-1", 1, []*types.SubnetTx{txRevealSeed(seedMsg)})
	_, err := sm.ApplyDiff(diff)
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
	execSig := testutil.SignExecutorReceipt(t, hosts[1], "escrow-1", 1, []byte("prompt"), "llama", 100, 50, 1000)

	finishMsg := &types.MsgFinishInference{
		InferenceId: 1, ResponseHash: []byte("response"),
		InputTokens: 80, OutputTokens: 40, ExecutorSlot: 2, // Wrong executor slot.
	}
	finishMsg.ProposerSig = testutil.SignProposerTx(t, hosts[2], finishMsg)

	diff = testutil.SignDiff(t, user, "escrow-1", 2, []*types.SubnetTx{
		txConfirm(&types.MsgConfirmStart{InferenceId: 1, ExecutorSig: execSig}),
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

	execSig := testutil.SignExecutorReceipt(t, hosts[executorSlotIdx], "escrow-1", inferenceID, []byte("prompt"), "llama", 100, 50, 1000)
	nonce++
	diff = testutil.SignDiff(t, user, "escrow-1", nonce, []*types.SubnetTx{txConfirm(&types.MsgConfirmStart{
		InferenceId: inferenceID, ExecutorSig: execSig,
	})})
	_, err = sm.ApplyDiff(diff)
	require.NoError(t, err)
}
