package bridge

import (
	"fmt"
	"subnet/types"
)

// BuildGroup fetches escrow data and host info to construct a session group.
// Slots come from the chain (stored in SubnetEscrow), no re-derivation needed.
func BuildGroup(escrowID string, b MainnetBridge) ([]types.SlotAssignment, error) {
	escrow, err := b.GetEscrow(escrowID)
	if err != nil {
		return nil, fmt.Errorf("get escrow: %w", err)
	}

	// Deduplicate addresses to avoid redundant queries.
	pubKeyCache := make(map[string][]byte)
	for _, addr := range escrow.Slots {
		if _, ok := pubKeyCache[addr]; ok {
			continue
		}
		pk, err := b.GetAccountPubKey(addr)
		if err != nil {
			return nil, fmt.Errorf("get account pubkey for %s: %w", addr, err)
		}
		pubKeyCache[addr] = pk
	}

	group := make([]types.SlotAssignment, len(escrow.Slots))
	for i, addr := range escrow.Slots {
		group[i] = types.SlotAssignment{
			SlotID:           uint32(i),
			ValidatorAddress: addr,
			PublicKey:        pubKeyCache[addr],
		}
	}

	if err := types.ValidateGroup(group); err != nil {
		return nil, err
	}
	return group, nil
}
