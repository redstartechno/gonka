package user

import (
	"context"
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
	PromptHash  []byte
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
	txs = append(txs, &types.SubnetTx{Tx: &types.SubnetTx_StartInference{
		StartInference: &types.MsgStartInference{
			InferenceId: nonce,
			Model:       params.Model,
			PromptHash:  params.PromptHash,
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
func (s *Session) ProcessResponse(hostIdx int, resp *host.HostResponse) error {
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
		s.pendingTxs = append(s.pendingTxs, &types.SubnetTx{
			Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
				InferenceId: resp.Nonce,
				ExecutorSig: resp.Receipt,
			}},
		})
	}

	// Queue mempool txs (finish msgs) for the next diff.
	s.pendingTxs = append(s.pendingTxs, resp.Mempool...)

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
	s.pendingTxs = nil

	// Build catch-up diffs for the target host.
	catchUpDiffs := s.DiffsForHost(hostIdx)

	resp, err := s.clients[hostIdx].Send(ctx, host.HostRequest{Diffs: catchUpDiffs})
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

func (s *Session) Signatures() map[uint64]map[uint32][]byte {
	return s.signatures
}

func (s *Session) Nonce() uint64 {
	return s.nonce
}

func (s *Session) Diffs() []types.Diff {
	return s.diffs
}

func (s *Session) StateMachine() *state.StateMachine {
	return s.sm
}
