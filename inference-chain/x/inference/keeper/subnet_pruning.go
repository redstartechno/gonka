package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/x/inference/types"
)

const SubnetPruningThreshold = uint64(2)

// PruneSubnetData deletes escrows (and related data) older than 2 epochs.
// Unsettled escrows split funds equally among the unique validators in the group.
func (k Keeper) PruneSubnetData(ctx context.Context, currentEpochIndex int64) error {
	if currentEpochIndex < int64(SubnetPruningThreshold) {
		return nil
	}

	lastPruned, err := k.SubnetPrunedEpoch.Get(ctx)
	if err != nil {
		lastPruned = -1
	}

	endEpoch := currentEpochIndex - int64(SubnetPruningThreshold)
	startEpoch := lastPruned + 1

	if startEpoch > endEpoch {
		return nil
	}

	for epoch := startEpoch; epoch <= endEpoch; epoch++ {
		if err := k.pruneSubnetEpoch(ctx, uint64(epoch)); err != nil {
			k.LogError("failed to prune subnet epoch", types.Pruning, "epoch", epoch, "error", err)
			continue
		}
	}

	return k.SubnetPrunedEpoch.Set(ctx, endEpoch)
}

func (k Keeper) pruneSubnetEpoch(ctx context.Context, epochIndex uint64) error {
	// Iterate SubnetEscrowsByEpoch for this epoch
	rng := collections.NewPrefixedPairRange[uint64, uint64](epochIndex)
	iter, err := k.SubnetEscrowsByEpoch.Iterate(ctx, rng)
	if err != nil {
		return fmt.Errorf("failed to iterate escrows by epoch: %w", err)
	}
	defer iter.Close()

	var escrowIDs []uint64
	for ; iter.Valid(); iter.Next() {
		key, err := iter.Key()
		if err != nil {
			return fmt.Errorf("failed to get escrow-by-epoch key: %w", err)
		}
		escrowIDs = append(escrowIDs, key.K2())
	}

	for _, escrowID := range escrowIDs {
		escrow, found := k.GetSubnetEscrow(ctx, escrowID)
		if found && !escrow.Settled {
			if err := k.distributeUnsettledEscrow(ctx, escrow); err != nil {
				k.LogError("failed to distribute unsettled escrow", types.Pruning,
					"escrow_id", escrowID, "error", err)
			}
		}

		// Delete escrow and index entry
		if err := k.SubnetEscrows.Remove(ctx, escrowID); err != nil {
			k.LogError("failed to remove subnet escrow", types.Pruning, "escrow_id", escrowID, "error", err)
		}
		if err := k.SubnetEscrowsByEpoch.Remove(ctx, collections.Join(epochIndex, escrowID)); err != nil {
			k.LogError("failed to remove subnet escrow index", types.Pruning, "escrow_id", escrowID, "error", err)
		}
	}

	// Clear SubnetHostEpochStats for this epoch
	statsRng := collections.NewPrefixedPairRange[uint64, sdk.AccAddress](epochIndex)
	statsIter, err := k.SubnetHostEpochStatsMap.Iterate(ctx, statsRng)
	if err == nil {
		defer statsIter.Close()
		for ; statsIter.Valid(); statsIter.Next() {
			key, err := statsIter.Key()
			if err != nil {
				continue
			}
			_ = k.SubnetHostEpochStatsMap.Remove(ctx, key)
		}
	}

	// Delete epoch count
	_ = k.SubnetEscrowEpochCount.Remove(ctx, epochIndex)

	return nil
}

// distributeUnsettledEscrow splits the escrowed funds equally among unique validators in the group.
// Integer division remainder stays in the module account.
func (k Keeper) distributeUnsettledEscrow(ctx context.Context, escrow types.SubnetEscrow) error {
	// Count unique addresses (first pass)
	seen := make(map[string]bool)
	var uniqueCount uint64
	for _, addr := range escrow.Slots {
		if !seen[addr] {
			seen[addr] = true
			uniqueCount++
		}
	}

	if uniqueCount == 0 {
		return nil
	}

	share := escrow.Amount / uniqueCount
	if share == 0 {
		return nil
	}

	// Pay in slot order (deterministic iteration over escrow.Slots)
	paid := make(map[string]bool)
	for _, addr := range escrow.Slots {
		if paid[addr] {
			continue
		}
		paid[addr] = true

		recipient, err := sdk.AccAddressFromBech32(addr)
		if err != nil {
			k.LogError("invalid address in unsettled escrow", types.Pruning,
				"escrow_id", escrow.Id, "address", addr)
			continue
		}
		coins, err := types.GetCoins(int64(share))
		if err != nil {
			continue
		}
		err = k.BankKeeper.SendCoinsFromModuleToAccount(ctx, types.ModuleName, recipient, coins, "subnet_escrow_unsettled_distribution")
		if err != nil {
			k.LogError("failed to distribute unsettled escrow funds", types.Pruning,
				"escrow_id", escrow.Id, "address", addr, "error", err)
		}
	}

	return nil
}
