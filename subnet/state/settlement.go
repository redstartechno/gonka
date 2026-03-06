package state

import (
	"subnet/types"
)

// SettlementPayload contains the data needed for on-chain settlement.
type SettlementPayload struct {
	StateRoot  []byte
	RestHash   []byte
	HostStats  map[uint32]*types.HostStats
	Signatures map[uint32][]byte
	Nonce      uint64
}

// BuildSettlement constructs a SettlementPayload from the final escrow state.
func BuildSettlement(st types.EscrowState, signatures map[uint32][]byte, nonce uint64) (*SettlementPayload, error) {
	hostStatsHash, err := ComputeHostStatsHash(st.HostStats)
	if err != nil {
		return nil, err
	}

	restHash, err := ComputeRestHash(st.Balance, st.Inferences)
	if err != nil {
		return nil, err
	}

	stateRoot, err := ComputeStateRoot(st.Balance, st.HostStats, st.Inferences)
	if err != nil {
		return nil, err
	}

	_ = hostStatsHash // used implicitly via stateRoot = sha256(hostStatsHash || restHash)

	return &SettlementPayload{
		StateRoot:  stateRoot,
		RestHash:   restHash,
		HostStats:  st.HostStats,
		Signatures: signatures,
		Nonce:      nonce,
	}, nil
}
