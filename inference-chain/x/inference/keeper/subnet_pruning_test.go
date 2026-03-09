package keeper_test

import (
	"testing"

	"cosmossdk.io/collections"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	keepertest "github.com/productscience/inference/testutil/keeper"
	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

func TestPruneSubnetData_DeletesOldEscrows(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")
	mock.BankKeeper.ExpectAny(ctx)

	// Create escrow in epoch 3
	escrow := &types.SubnetEscrow{
		Creator:    "gonka1creator",
		Amount:     5_000_000_000,
		Slots:      make([]string, 16),
		EpochIndex: 3,
		Settled:    true,
	}
	id, err := k.StoreSubnetEscrow(ctx, escrow, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), id)

	// Verify escrow exists
	_, found := k.GetSubnetEscrow(ctx, 1)
	require.True(t, found)

	// Prune at epoch 5 (threshold=2, so epoch 3 should be pruned)
	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)

	// Escrow should be deleted
	_, found = k.GetSubnetEscrow(ctx, 1)
	require.False(t, found)
}

func TestPruneSubnetData_PreservesRecentEscrows(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")
	mock.BankKeeper.ExpectAny(ctx)

	// Create escrow in epoch 4
	escrow := &types.SubnetEscrow{
		Creator:    "gonka1creator",
		Amount:     5_000_000_000,
		Slots:      make([]string, 16),
		EpochIndex: 4,
		Settled:    true,
	}
	_, err := k.StoreSubnetEscrow(ctx, escrow, 1)
	require.NoError(t, err)

	// Prune at epoch 5 (threshold=2, so epoch 4 is not yet prunable)
	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)

	// Escrow should still exist
	_, found := k.GetSubnetEscrow(ctx, 1)
	require.True(t, found)
}

func TestPruneSubnetData_HostStatsDeleted(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")
	mock.BankKeeper.ExpectAny(ctx)

	participant := sdk.AccAddress(make([]byte, 20))
	participant[0] = 0x01

	// Create escrow and stats for epoch 3
	escrow := &types.SubnetEscrow{
		Creator:    "gonka1creator",
		Amount:     5_000_000_000,
		Slots:      make([]string, 16),
		EpochIndex: 3,
		Settled:    true,
	}
	_, err := k.StoreSubnetEscrow(ctx, escrow, 1)
	require.NoError(t, err)

	_ = k.SubnetHostEpochStatsMap.Set(ctx, collections.Join(uint64(3), participant), types.SubnetHostEpochStats{
		Participant: participant.String(),
		EpochIndex:  3,
		Cost:        100,
		EscrowCount: 1,
	})

	// Prune at epoch 5
	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)

	// Stats should be deleted
	_, found := k.GetSubnetHostEpochStats(ctx, 3, participant)
	require.False(t, found)

	// Epoch count should be deleted
	count := k.GetSubnetEscrowEpochCount(ctx, 3)
	require.Equal(t, uint64(0), count)
}

func TestPruneSubnetData_UnsettledEscrowDistributesFunds(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")

	// Create 4 unique validators in 16 slots
	addr1 := sdk.AccAddress(make([]byte, 20))
	addr1[0] = 0x01
	addr2 := sdk.AccAddress(make([]byte, 20))
	addr2[0] = 0x02
	addr3 := sdk.AccAddress(make([]byte, 20))
	addr3[0] = 0x03
	addr4 := sdk.AccAddress(make([]byte, 20))
	addr4[0] = 0x04

	slots := make([]string, keeper.SubnetGroupSize)
	for i := 0; i < 4; i++ {
		slots[i] = addr1.String()
	}
	for i := 4; i < 8; i++ {
		slots[i] = addr2.String()
	}
	for i := 8; i < 12; i++ {
		slots[i] = addr3.String()
	}
	for i := 12; i < 16; i++ {
		slots[i] = addr4.String()
	}

	escrow := &types.SubnetEscrow{
		Creator:    "gonka1creator",
		Amount:     8_000_000_000, // 8 GNK
		Slots:      slots,
		EpochIndex: 3,
		Settled:    false, // unsettled
	}
	_, err := k.StoreSubnetEscrow(ctx, escrow, 1)
	require.NoError(t, err)

	// Expect 4 payments of 2 GNK each (8 GNK / 4 unique validators)
	mock.BankKeeper.EXPECT().
		SendCoinsFromModuleToAccount(gomock.Any(), types.ModuleName, gomock.Any(), gomock.Any(), gomock.Eq("subnet_escrow_unsettled_distribution")).
		Return(nil).
		Times(4)

	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)

	// Escrow should be deleted
	_, found := k.GetSubnetEscrow(ctx, 1)
	require.False(t, found)
}

func TestPruneSubnetData_UnsettledDistributionAmounts(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")

	// Create 4 unique validators in 16 slots (4 slots each)
	addrs := make([]sdk.AccAddress, 4)
	for i := range addrs {
		addrs[i] = sdk.AccAddress(make([]byte, 20))
		addrs[i][0] = byte(i + 1)
	}

	slots := make([]string, keeper.SubnetGroupSize)
	for i := 0; i < 4; i++ {
		slots[i] = addrs[0].String()
	}
	for i := 4; i < 8; i++ {
		slots[i] = addrs[1].String()
	}
	for i := 8; i < 12; i++ {
		slots[i] = addrs[2].String()
	}
	for i := 12; i < 16; i++ {
		slots[i] = addrs[3].String()
	}

	escrow := &types.SubnetEscrow{
		Creator:    "gonka1creator",
		Amount:     8_000_000_000, // 8 GNK
		Slots:      slots,
		EpochIndex: 3,
		Settled:    false,
	}
	_, err := k.StoreSubnetEscrow(ctx, escrow, 1)
	require.NoError(t, err)

	// Each of 4 validators should receive exactly 2 GNK (8 GNK / 4)
	expectedShare, err := types.GetCoins(2_000_000_000)
	require.NoError(t, err)

	for _, addr := range addrs {
		mock.BankKeeper.EXPECT().
			SendCoinsFromModuleToAccount(gomock.Any(), types.ModuleName, addr, expectedShare, gomock.Eq("subnet_escrow_unsettled_distribution")).
			Return(nil)
	}

	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)
}

func TestPruneSubnetData_TracksProgress(t *testing.T) {
	k, ctx, mock := keepertest.InferenceKeeperReturningMocks(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")
	mock.BankKeeper.ExpectAny(ctx)

	// Create escrows in epochs 1, 2, 3
	for epoch := uint64(1); epoch <= 3; epoch++ {
		escrow := &types.SubnetEscrow{
			Creator:    "gonka1creator",
			Amount:     5_000_000_000,
			Slots:      make([]string, 16),
			EpochIndex: epoch,
			Settled:    true,
		}
		_, err := k.StoreSubnetEscrow(ctx, escrow, epoch)
		require.NoError(t, err)
	}

	// Prune at epoch 4 -> should prune epochs 1 and 2
	err := k.PruneSubnetData(ctx, 4)
	require.NoError(t, err)

	lastPruned, err := k.SubnetPrunedEpoch.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), lastPruned)

	// Epoch 3 escrow should still exist
	_, found := k.GetSubnetEscrow(ctx, 3)
	require.True(t, found)

	// Prune at epoch 5 -> should prune epoch 3
	err = k.PruneSubnetData(ctx, 5)
	require.NoError(t, err)

	lastPruned, err = k.SubnetPrunedEpoch.Get(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), lastPruned)

	_, found = k.GetSubnetEscrow(ctx, 3)
	require.False(t, found)
}
