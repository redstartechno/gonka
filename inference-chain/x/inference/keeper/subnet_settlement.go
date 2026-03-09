package keeper

import (
	"crypto/sha256"
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/productscience/inference/x/inference/types"
)

const (
	SubnetGroupSize      = 16
	SubnetQuorumSlots    = 11 // 2*16/3 + 1
	SubnetFinalizedPhase = byte(0x02)
)

// VerifySubnetSettlement verifies the settlement proof against the escrow.
// It checks the state root, signatures, quorum, and cost constraints.
func VerifySubnetSettlement(escrow types.SubnetEscrow, msg *types.MsgSettleSubnetEscrow) error {
	if escrow.Settled {
		return fmt.Errorf("escrow %d already settled", escrow.Id)
	}
	if msg.Settler != escrow.Creator {
		return fmt.Errorf("settler %s is not the escrow creator %s", msg.Settler, escrow.Creator)
	}

	// Recompute host_stats_hash
	hostStatsHash, err := computeSubnetHostStatsHash(msg.HostStats)
	if err != nil {
		return fmt.Errorf("failed to compute host stats hash: %w", err)
	}

	// Verify state_root = sha256(host_stats_hash || rest_hash || 0x02)
	rootInput := make([]byte, 0, len(hostStatsHash)+len(msg.RestHash)+1)
	rootInput = append(rootInput, hostStatsHash...)
	rootInput = append(rootInput, msg.RestHash...)
	rootInput = append(rootInput, SubnetFinalizedPhase)
	expectedRoot := sha256.Sum256(rootInput)
	if len(msg.StateRoot) != 32 {
		return fmt.Errorf("state_root must be 32 bytes, got %d", len(msg.StateRoot))
	}
	for i := range expectedRoot {
		if expectedRoot[i] != msg.StateRoot[i] {
			return fmt.Errorf("state_root mismatch")
		}
	}

	// Build signature data using deterministic proto marshal
	sigContent := &types.SubnetStateSignatureContent{
		StateRoot: msg.StateRoot,
		EscrowId:  fmt.Sprint(escrow.Id),
		Nonce:     msg.Nonce,
	}
	sigData, err := deterministicMarshal(sigContent)
	if err != nil {
		return fmt.Errorf("failed to marshal sig content: %w", err)
	}
	sigHash := sha256.Sum256(sigData)

	// Verify signatures and count slot votes
	slotVotes := 0
	for _, sig := range msg.Signatures {
		if int(sig.SlotId) >= len(escrow.Slots) {
			return fmt.Errorf("slot_id %d out of range", sig.SlotId)
		}
		expectedAddr := escrow.Slots[sig.SlotId]

		recovered, err := recoverCosmosAddress(sigHash[:], sig.Signature)
		if err != nil {
			return fmt.Errorf("failed to recover address for slot %d: %w", sig.SlotId, err)
		}
		if recovered.String() != expectedAddr {
			return fmt.Errorf("signature for slot %d recovered %s, expected %s", sig.SlotId, recovered.String(), expectedAddr)
		}

		slotVotes++
	}

	// Check quorum: need >= 11 slot votes
	if slotVotes < SubnetQuorumSlots {
		return fmt.Errorf("insufficient quorum: %d slot votes, need %d", slotVotes, SubnetQuorumSlots)
	}

	// Verify total cost does not exceed escrow amount
	var totalCost uint64
	for _, hs := range msg.HostStats {
		totalCost += hs.Cost
	}
	if totalCost > escrow.Amount {
		return fmt.Errorf("total cost %d exceeds escrow amount %d", totalCost, escrow.Amount)
	}

	return nil
}

// deterministicMarshal uses gogoproto's XXX_Marshal with deterministic=true.
// This produces the same bytes as google.golang.org/protobuf's deterministic marshal
// for proto3 messages (fields serialized in field number order).
func deterministicMarshal(msg interface {
	XXX_Marshal(b []byte, deterministic bool) ([]byte, error)
}) ([]byte, error) {
	return msg.XXX_Marshal(nil, true)
}

// computeSubnetHostStatsHash recomputes the host stats hash from settlement host stats.
// Uses the same proto deterministic marshal as the subnet module.
func computeSubnetHostStatsHash(hostStats []*types.SubnetSettlementHostStats) ([]byte, error) {
	entries := make([]*types.SubnetHostStatsProto, len(hostStats))
	for i, hs := range hostStats {
		entries[i] = &types.SubnetHostStatsProto{
			SlotId:               hs.SlotId,
			Missed:               hs.Missed,
			Invalid:              hs.Invalid,
			Cost:                 hs.Cost,
			RequiredValidations:  hs.RequiredValidations,
			CompletedValidations: hs.CompletedValidations,
		}
	}
	mapProto := &types.SubnetHostStatsMapProto{Entries: entries}
	data, err := deterministicMarshal(mapProto)
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}

// recoverCosmosAddress recovers a Cosmos bech32 address from a secp256k1 signature.
// The signature is in go-ethereum format: [R(32) || S(32) || V(1)].
// dcrd expects [V+27(1) || R(32) || S(32)].
func recoverCosmosAddress(hash []byte, sig []byte) (sdk.AccAddress, error) {
	if len(sig) != 65 {
		return nil, fmt.Errorf("signature must be 65 bytes, got %d", len(sig))
	}

	// Convert go-ethereum format [R(32)||S(32)||V(1)] to dcrd format [V+27(1)||R(32)||S(32)]
	v := sig[64]
	dcrdSig := make([]byte, 65)
	dcrdSig[0] = v + 27
	copy(dcrdSig[1:33], sig[0:32])
	copy(dcrdSig[33:65], sig[32:64])

	pubKey, _, err := ecdsa.RecoverCompact(dcrdSig, hash)
	if err != nil {
		return nil, fmt.Errorf("ecrecover failed: %w", err)
	}

	// Derive Cosmos address: SHA256(compressed_pubkey)[:20]
	compressed := pubKey.SerializeCompressed()
	addrHash := sha256.Sum256(compressed)
	return sdk.AccAddress(addrHash[:20]), nil
}
