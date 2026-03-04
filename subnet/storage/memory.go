package storage

import (
	"fmt"
	"sync"

	"subnet/types"
)

type sessionData struct {
	escrowID     string
	config       types.SessionConfig
	group        []types.SlotAssignment
	balance      uint64
	diffs        []types.DiffRecord
	nonceToIndex map[uint64]int
}

// Memory is an in-memory storage implementation for testing.
type Memory struct {
	mu       sync.RWMutex
	sessions map[string]*sessionData
}

func NewMemory() *Memory {
	return &Memory{
		sessions: make(map[string]*sessionData),
	}
}

func (m *Memory) CreateSession(escrowID string, config types.SessionConfig, group []types.SlotAssignment, balance uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.sessions[escrowID]; exists {
		return fmt.Errorf("session %s already exists", escrowID)
	}

	groupCopy := make([]types.SlotAssignment, len(group))
	for i, sa := range group {
		cp := sa
		if sa.PublicKey != nil {
			cp.PublicKey = make([]byte, len(sa.PublicKey))
			copy(cp.PublicKey, sa.PublicKey)
		}
		groupCopy[i] = cp
	}

	m.sessions[escrowID] = &sessionData{
		escrowID:     escrowID,
		config:       config,
		group:        groupCopy,
		balance:      balance,
		nonceToIndex: make(map[uint64]int),
	}
	return nil
}

func (m *Memory) AppendDiff(escrowID string, rec types.DiffRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return fmt.Errorf("session %s not found", escrowID)
	}

	// Deep copy signatures map.
	sigsCopy := make(map[uint32][]byte, len(rec.Signatures))
	for k, v := range rec.Signatures {
		vc := make([]byte, len(v))
		copy(vc, v)
		sigsCopy[k] = vc
	}
	rec.Signatures = sigsCopy

	s.diffs = append(s.diffs, rec)
	s.nonceToIndex[rec.Nonce] = len(s.diffs) - 1
	return nil
}

func (m *Memory) AddSignature(escrowID string, nonce uint64, slotID uint32, sig []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return fmt.Errorf("session %s not found", escrowID)
	}

	idx, ok := s.nonceToIndex[nonce]
	if !ok {
		return fmt.Errorf("diff at nonce %d not found for session %s", nonce, escrowID)
	}
	if s.diffs[idx].Signatures == nil {
		s.diffs[idx].Signatures = make(map[uint32][]byte)
	}
	sc := make([]byte, len(sig))
	copy(sc, sig)
	s.diffs[idx].Signatures[slotID] = sc
	return nil
}

func (m *Memory) GetState(escrowID string) (*types.EscrowState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", escrowID)
	}

	groupCopy := make([]types.SlotAssignment, len(s.group))
	for i, sa := range s.group {
		cp := sa
		if sa.PublicKey != nil {
			cp.PublicKey = make([]byte, len(sa.PublicKey))
			copy(cp.PublicKey, sa.PublicKey)
		}
		groupCopy[i] = cp
	}

	state := &types.EscrowState{
		EscrowID: s.escrowID,
		Config:   s.config,
		Group:    groupCopy,
		Balance:  s.balance,
	}

	if len(s.diffs) > 0 {
		state.LatestNonce = s.diffs[len(s.diffs)-1].Nonce
	}

	return state, nil
}

func (m *Memory) GetDiffs(escrowID string, fromNonce, toNonce uint64) ([]types.DiffRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", escrowID)
	}

	var result []types.DiffRecord
	for _, d := range s.diffs {
		if d.Nonce < fromNonce || d.Nonce > toNonce {
			continue
		}
		// Deep copy signatures.
		sigsCopy := make(map[uint32][]byte, len(d.Signatures))
		for k, v := range d.Signatures {
			vc := make([]byte, len(v))
			copy(vc, v)
			sigsCopy[k] = vc
		}
		dc := d
		dc.Signatures = sigsCopy
		result = append(result, dc)
	}

	return result, nil
}
