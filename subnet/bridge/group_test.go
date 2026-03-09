package bridge

import (
	"subnet/types"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockBridge implements MainnetBridge for testing BuildGroup.
type mockBridge struct {
	escrow     *EscrowInfo
	escrowErr  error
	validators map[string]*ValidatorInfo
	validErr   error
}

func (m *mockBridge) GetEscrow(_ string) (*EscrowInfo, error) {
	return m.escrow, m.escrowErr
}
func (m *mockBridge) GetValidatorInfo(addr string) (*ValidatorInfo, error) {
	if m.validErr != nil {
		return nil, m.validErr
	}
	info, ok := m.validators[addr]
	if !ok {
		return nil, ErrParticipantNotFound
	}
	return info, nil
}
func (m *mockBridge) VerifyWarmKey(_, _ string) (bool, error) {
	return false, ErrNotImplemented
}
func (m *mockBridge) OnEscrowCreated(_ EscrowInfo) error                            { return ErrNotImplemented }
func (m *mockBridge) OnSettlementProposed(_ string, _ []byte, _ uint64) error       { return ErrNotImplemented }
func (m *mockBridge) OnSettlementFinalized(_ string) error                          { return ErrNotImplemented }
func (m *mockBridge) SubmitDisputeState(_ string, _ []byte, _ uint64, _ map[uint32][]byte) error {
	return ErrNotImplemented
}

func TestBuildGroup_HappyPath(t *testing.T) {
	b := &mockBridge{
		escrow: &EscrowInfo{
			EscrowID: "1",
			Slots:    []string{"valA", "valB", "valC"},
		},
		validators: map[string]*ValidatorInfo{
			"valA": {Address: "valA", PublicKey: []byte{1}, Weight: 10},
			"valB": {Address: "valB", PublicKey: []byte{2}, Weight: 20},
			"valC": {Address: "valC", PublicKey: []byte{3}, Weight: 30},
		},
	}

	group, err := BuildGroup("1", b)
	require.NoError(t, err)
	require.Len(t, group, 3)

	for i, slot := range group {
		assert.Equal(t, uint32(i), slot.SlotID)
	}
	assert.Equal(t, "valA", group[0].ValidatorAddress)
	assert.Equal(t, "valC", group[2].ValidatorAddress)
	assert.Equal(t, uint64(20), group[1].Weight)
}

func TestBuildGroup_Deduplication(t *testing.T) {
	queriedAddrs := make(map[string]int)

	b := &mockBridge{
		escrow: &EscrowInfo{
			EscrowID: "1",
			// valA appears in slots 0, 1, and 3
			Slots: []string{"valA", "valA", "valB", "valA"},
		},
		validators: map[string]*ValidatorInfo{
			"valA": {Address: "valA", PublicKey: []byte{1}, Weight: 10},
			"valB": {Address: "valB", PublicKey: []byte{2}, Weight: 20},
		},
	}

	// Wrap to count queries
	wrapper := &queryCountBridge{inner: b, counts: queriedAddrs}

	group, err := BuildGroup("1", wrapper)
	require.NoError(t, err)
	require.Len(t, group, 4)

	// valA should only be queried once despite appearing 3 times
	assert.Equal(t, 1, queriedAddrs["valA"])
	assert.Equal(t, 1, queriedAddrs["valB"])

	// All slots should have correct SlotID
	for i, slot := range group {
		assert.Equal(t, uint32(i), slot.SlotID)
	}
	// Slots 0, 1, 3 all map to valA
	assert.Equal(t, "valA", group[0].ValidatorAddress)
	assert.Equal(t, "valA", group[1].ValidatorAddress)
	assert.Equal(t, "valB", group[2].ValidatorAddress)
	assert.Equal(t, "valA", group[3].ValidatorAddress)
}

func TestBuildGroup_EscrowError(t *testing.T) {
	b := &mockBridge{escrowErr: ErrEscrowNotFound}
	_, err := BuildGroup("1", b)
	assert.ErrorIs(t, err, ErrEscrowNotFound)
}

func TestBuildGroup_ValidatorError(t *testing.T) {
	b := &mockBridge{
		escrow: &EscrowInfo{
			EscrowID: "1",
			Slots:    []string{"valA", "missing"},
		},
		validators: map[string]*ValidatorInfo{
			"valA": {Address: "valA", PublicKey: []byte{1}, Weight: 10},
		},
	}
	_, err := BuildGroup("1", b)
	assert.ErrorIs(t, err, ErrParticipantNotFound)
}

// queryCountBridge wraps a MainnetBridge to count GetValidatorInfo calls.
type queryCountBridge struct {
	inner  MainnetBridge
	counts map[string]int
}

func (q *queryCountBridge) GetEscrow(id string) (*EscrowInfo, error) {
	return q.inner.GetEscrow(id)
}
func (q *queryCountBridge) GetValidatorInfo(addr string) (*ValidatorInfo, error) {
	q.counts[addr]++
	return q.inner.GetValidatorInfo(addr)
}
func (q *queryCountBridge) VerifyWarmKey(w, v string) (bool, error) {
	return q.inner.VerifyWarmKey(w, v)
}
func (q *queryCountBridge) OnEscrowCreated(e EscrowInfo) error { return q.inner.OnEscrowCreated(e) }
func (q *queryCountBridge) OnSettlementProposed(id string, sr []byte, n uint64) error {
	return q.inner.OnSettlementProposed(id, sr, n)
}
func (q *queryCountBridge) OnSettlementFinalized(id string) error {
	return q.inner.OnSettlementFinalized(id)
}
func (q *queryCountBridge) SubmitDisputeState(id string, sr []byte, n uint64, sigs map[uint32][]byte) error {
	return q.inner.SubmitDisputeState(id, sr, n, sigs)
}

func TestBuildGroup_ValidateGroupPasses(t *testing.T) {
	b := &mockBridge{
		escrow: &EscrowInfo{
			EscrowID: "1",
			Slots:    []string{"valA"},
		},
		validators: map[string]*ValidatorInfo{
			"valA": {Address: "valA", PublicKey: []byte{1}, Weight: 10},
		},
	}

	group, err := BuildGroup("1", b)
	require.NoError(t, err)
	// ValidateGroup is called inside BuildGroup, but verify directly too
	assert.NoError(t, types.ValidateGroup(group))
}
