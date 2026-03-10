package storage

import (
	"fmt"
	"sync"

	"subnet/types"
)

func copySignatures(src map[uint32][]byte) map[uint32][]byte {
	if src == nil {
		return nil
	}
	dst := make(map[uint32][]byte, len(src))
	for k, v := range src {
		dst[k] = append([]byte(nil), v...)
	}
	return dst
}

func copyGroup(src []types.SlotAssignment) []types.SlotAssignment {
	dst := make([]types.SlotAssignment, len(src))
	for i, s := range src {
		dst[i] = s
		if s.PublicKey != nil {
			dst[i].PublicKey = append([]byte(nil), s.PublicKey...)
		}
	}
	return dst
}

type sessionData struct {
	escrowID      string
	config        types.SessionConfig
	group         []types.SlotAssignment
	balance       uint64
	diffs         []types.DiffRecord
	nonceToIndex  map[uint64]int
	lastFinalized uint64
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

	m.sessions[escrowID] = &sessionData{
		escrowID:     escrowID,
		config:       config,
		group:        copyGroup(group),
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

	rec.Signatures = copySignatures(rec.Signatures)

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

	state := &types.EscrowState{
		EscrowID: s.escrowID,
		Config:   s.config,
		Group:    copyGroup(s.group),
		Balance:  s.balance,
	}

	if len(s.diffs) > 0 {
		state.LatestNonce = s.diffs[len(s.diffs)-1].Nonce
	}

	return state, nil
}

func (m *Memory) GetSignatures(escrowID string, nonce uint64) (map[uint32][]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return nil, fmt.Errorf("session %s not found", escrowID)
	}

	idx, ok := s.nonceToIndex[nonce]
	if !ok {
		return nil, fmt.Errorf("diff at nonce %d not found for session %s", nonce, escrowID)
	}

	return copySignatures(s.diffs[idx].Signatures), nil
}

func (m *Memory) MarkFinalized(escrowID string, nonce uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return fmt.Errorf("session %s not found", escrowID)
	}

	if nonce > s.lastFinalized {
		s.lastFinalized = nonce
	}
	return nil
}

func (m *Memory) LastFinalized(escrowID string) (uint64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[escrowID]
	if !ok {
		return 0, fmt.Errorf("session %s not found", escrowID)
	}

	return s.lastFinalized, nil
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
		dc := d
		dc.Signatures = copySignatures(d.Signatures)
		result = append(result, dc)
	}

	return result, nil
}
