package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"

	"google.golang.org/protobuf/proto"

	"subnet/types"
)

// ComputeStateRoot computes the two-level Merkle state hash:
//
//	          state_root
//	         /          \
//	host_stats_hash    rest_hash
//
// host_stats_hash = sha256(proto(sorted host stats))
// inferences_hash = sha256(proto(sorted inference records))
// rest_hash       = sha256(balance_be || inferences_hash)
// state_root      = sha256(host_stats_hash || rest_hash)
func ComputeStateRoot(balance uint64, hostStats map[uint32]*types.HostStats, inferences map[uint64]*types.InferenceRecord) ([]byte, error) {
	hostStatsHash, err := computeHostStatsHash(hostStats)
	if err != nil {
		return nil, err
	}
	restHash, err := computeRestHash(balance, inferences)
	if err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write(hostStatsHash)
	h.Write(restHash)
	return h.Sum(nil), nil
}

// ComputeHostStatsHash computes sha256(proto(sorted host stats)).
// Exported for settlement verification on mainnet.
func ComputeHostStatsHash(hostStats map[uint32]*types.HostStats) ([]byte, error) {
	return computeHostStatsHash(hostStats)
}

// ComputeRestHash computes sha256(balance_be || inferences_hash).
// Exported for settlement verification on mainnet.
func ComputeRestHash(balance uint64, inferences map[uint64]*types.InferenceRecord) ([]byte, error) {
	return computeRestHash(balance, inferences)
}

func computeHostStatsHash(hostStats map[uint32]*types.HostStats) ([]byte, error) {
	// Sort slot IDs for determinism.
	slotIDs := make([]uint32, 0, len(hostStats))
	for id := range hostStats {
		slotIDs = append(slotIDs, id)
	}
	slices.SortFunc(slotIDs, func(a, b uint32) int { return cmp.Compare(a, b) })

	entries := make([]*types.HostStatsProto, 0, len(slotIDs))
	for _, id := range slotIDs {
		s := hostStats[id]
		entries = append(entries, &types.HostStatsProto{
			SlotId:               id,
			Missed:               s.Missed,
			Invalid:              s.Invalid,
			Cost:                 s.Cost,
			RequiredValidations:  s.RequiredValidations,
			CompletedValidations: s.CompletedValidations,
		})
	}

	msg := &types.HostStatsMapProto{Entries: entries}
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal host stats: %w", err)
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}

func computeRestHash(balance uint64, inferences map[uint64]*types.InferenceRecord) ([]byte, error) {
	infHash, err := computeInferencesHash(inferences)
	if err != nil {
		return nil, err
	}

	balBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(balBytes, balance)

	h := sha256.New()
	h.Write(balBytes)
	h.Write(infHash)
	return h.Sum(nil), nil
}

func computeInferencesHash(inferences map[uint64]*types.InferenceRecord) ([]byte, error) {
	// Sort inference IDs for determinism.
	ids := make([]uint64, 0, len(inferences))
	for id := range inferences {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uint64) int { return cmp.Compare(a, b) })

	entries := make([]*types.InferenceRecordProto, 0, len(ids))
	for _, id := range ids {
		r := inferences[id]

		// Sort VotedSlots for deterministic hashing.
		var votedSlots []uint32
		for slot := range r.VotedSlots {
			votedSlots = append(votedSlots, slot)
		}
		slices.SortFunc(votedSlots, func(a, b uint32) int { return cmp.Compare(a, b) })

		entries = append(entries, &types.InferenceRecordProto{
			InferenceId:  id,
			Status:       uint32(r.Status),
			ExecutorSlot: r.ExecutorSlot,
			Model:        r.Model,
			PromptHash:   r.PromptHash,
			ResponseHash: r.ResponseHash,
			InputLength:  r.InputLength,
			MaxTokens:    r.MaxTokens,
			InputTokens:  r.InputTokens,
			OutputTokens: r.OutputTokens,
			ReservedCost: r.ReservedCost,
			ActualCost:   r.ActualCost,
			StartedAt:    r.StartedAt,
			VotesValid:   r.VotesValid,
			VotesInvalid: r.VotesInvalid,
			VotedSlots:   votedSlots,
		})
	}

	msg := &types.InferencesMapProto{Entries: entries}
	data, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("marshal inferences: %w", err)
	}
	hash := sha256.Sum256(data)
	return hash[:], nil
}
