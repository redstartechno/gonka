package testutil

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"subnet/signing"
	"subnet/types"
)

var TestPrompt = []byte("prompt")
var TestPromptHash = sha256.Sum256(TestPrompt)

func MustGenerateKey(t *testing.T) *signing.Secp256k1Signer {
	t.Helper()
	s, err := signing.GenerateKey()
	require.NoError(t, err)
	return s
}

func MakeGroup(signers []*signing.Secp256k1Signer) []types.SlotAssignment {
	group := make([]types.SlotAssignment, len(signers))
	for i, s := range signers {
		group[i] = types.SlotAssignment{
			SlotID:           uint32(i),
			ValidatorAddress: s.Address(),
			PublicKey:        s.PublicKeyBytes(),
			Weight:           1,
		}
	}
	return group
}

// DefaultConfig returns a SessionConfig with VoteThreshold = numHosts/2.
func DefaultConfig(numHosts int) types.SessionConfig {
	return types.SessionConfig{
		RefusalTimeout:   60,
		ExecutionTimeout: 1200,
		TokenPrice:       1,
		VoteThreshold:    uint32(numHosts) / 2,
	}
}

func SignDiff(t *testing.T, signer signing.Signer, escrowID string, nonce uint64, txs []*types.SubnetTx) types.Diff {
	t.Helper()
	content := &types.DiffContent{Nonce: nonce, Txs: txs, EscrowId: escrowID}
	data, err := proto.Marshal(content)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return types.Diff{Nonce: nonce, Txs: txs, UserSig: sig}
}

func SignProposerTx(t *testing.T, signer signing.Signer, msg proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(msg)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return sig
}

func SignExecutorReceipt(t *testing.T, signer signing.Signer, escrowID string, inferenceID uint64, promptHash []byte, model string, inputLength, maxTokens uint64, startedAt int64) []byte {
	t.Helper()
	content := &types.ExecutorReceiptContent{
		InferenceId: inferenceID,
		PromptHash:  promptHash,
		Model:       model,
		InputLength: inputLength,
		MaxTokens:   maxTokens,
		StartedAt:   startedAt,
		EscrowId:    escrowID,
	}
	data, err := proto.Marshal(content)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return sig
}

func SignTimeoutVote(t *testing.T, signer signing.Signer, escrowID string, inferenceID uint64, reason types.TimeoutReason, accept bool) *types.TimeoutVote {
	t.Helper()
	content := &types.TimeoutVoteContent{
		EscrowId:    escrowID,
		InferenceId: inferenceID,
		Reason:      reason,
		Accept:      accept,
	}
	data, err := proto.Marshal(content)
	require.NoError(t, err)
	sig, err := signer.Sign(data)
	require.NoError(t, err)
	return &types.TimeoutVote{
		Accept:    accept,
		Signature: sig,
	}
}

func StartTx(inferenceID uint64) *types.SubnetTx {
	return &types.SubnetTx{Tx: &types.SubnetTx_StartInference{StartInference: &types.MsgStartInference{
		InferenceId: inferenceID,
		PromptHash:  TestPromptHash[:],
		Model:       "llama",
		InputLength: 100,
		MaxTokens:   50,
		StartedAt:   1000,
	}}}
}
