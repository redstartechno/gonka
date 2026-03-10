package bridge

import (
	"fmt"
	"subnet/types"
)

// BuildGroup fetches escrow data and validator info to construct a session group.
// Slots come from the chain (stored in SubnetEscrow), no re-derivation needed.
func BuildGroup(escrowID string, b MainnetBridge) ([]types.SlotAssignment, error) {
	escrow, err := b.GetEscrow(escrowID)
	if err != nil {
		return nil, fmt.Errorf("get escrow: %w", err)
	}

	// Deduplicate addresses to avoid redundant queries.
	infoCache := make(map[string]*ValidatorInfo)
	for _, addr := range escrow.Slots {
		if _, ok := infoCache[addr]; ok {
			continue
		}
		info, err := b.GetValidatorInfo(addr)
		if err != nil {
			return nil, fmt.Errorf("get validator info for %s: %w", addr, err)
		}
		infoCache[addr] = info
	}

	group := make([]types.SlotAssignment, len(escrow.Slots))
	for i, addr := range escrow.Slots {
		info := infoCache[addr]
		group[i] = types.SlotAssignment{
			SlotID:           uint32(i),
			ValidatorAddress: addr,
			PublicKey:        info.PublicKey,
		}
	}

	if err := types.ValidateGroup(group); err != nil {
		return nil, err
	}
	return group, nil
}
