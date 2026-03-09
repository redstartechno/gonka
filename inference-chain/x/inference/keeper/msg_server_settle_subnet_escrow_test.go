package keeper_test

import (
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	dcrdsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"

	"github.com/productscience/inference/x/inference/keeper"
	"github.com/productscience/inference/x/inference/types"
)

func TestSettleSubnetEscrow_HappyPath(t *testing.T) {
	k, ms, ctx, mocks := setupSubnetEscrowTest(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")

	keys := make([]*dcrdsecp.PrivateKey, keeper.SubnetGroupSize)
	slots := make([]string, keeper.SubnetGroupSize)
	for i := 0; i < keeper.SubnetGroupSize; i++ {
		key, err := dcrdsecp.GeneratePrivateKey()
		require.NoError(t, err)
		keys[i] = key
		slots[i] = cosmosAddressFromDcrdKey(key).String()
	}

	creator := sdk.AccAddress(make([]byte, 20))
	creator[0] = 0xAA
	escrow := types.SubnetEscrow{
		Id:         1,
		Creator:    creator.String(),
		Amount:     7_000_000_000,
		Slots:      slots,
		EpochIndex: 5,
		Settled:    false,
	}
	_, err := k.StoreSubnetEscrow(ctx, &escrow)
	require.NoError(t, err)

	costPerSlot := uint64(100_000_000) // 0.1 GNK per slot
	hostStats := makeHostStats(keeper.SubnetGroupSize, costPerSlot)
	msg := buildSettlementTestData(t, escrow, keys, hostStats)

	// Expect payments to validators (deduplicated by address)
	mocks.BankKeeper.EXPECT().
		SendCoinsFromModuleToAccount(gomock.Any(), types.ModuleName, gomock.Any(), gomock.Any(), gomock.Eq("subnet_escrow_payment")).
		Return(nil).
		Times(keeper.SubnetGroupSize) // 16 unique validators

	// Expect refund to creator
	totalCost := costPerSlot * uint64(keeper.SubnetGroupSize) // 1.6 GNK
	remainder := escrow.Amount - totalCost                     // 5.4 GNK
	_ = remainder
	mocks.BankKeeper.EXPECT().
		SendCoinsFromModuleToAccount(gomock.Any(), types.ModuleName, creator, gomock.Any(), gomock.Eq("subnet_escrow_refund")).
		Return(nil)

	resp, err := ms.SettleSubnetEscrow(ctx, msg)
	require.NoError(t, err)
	require.NotNil(t, resp)

	// Verify escrow is settled
	settled, found := k.GetSubnetEscrow(ctx, 1)
	require.True(t, found)
	require.True(t, settled.Settled)
}

func TestSettleSubnetEscrow_AlreadySettled(t *testing.T) {
	k, ms, ctx, _ := setupSubnetEscrowTest(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")

	escrow := types.SubnetEscrow{
		Id:      1,
		Creator: "gonka1creator",
		Settled: true,
		Slots:   make([]string, keeper.SubnetGroupSize),
	}
	_, err := k.StoreSubnetEscrow(ctx, &escrow)
	require.NoError(t, err)

	_, err = ms.SettleSubnetEscrow(ctx, &types.MsgSettleSubnetEscrow{
		Settler:  "gonka1creator",
		EscrowId: 1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "already settled")
}

func TestSettleSubnetEscrow_WrongSettler(t *testing.T) {
	k, ms, ctx, _ := setupSubnetEscrowTest(t)
	sdk.GetConfig().SetBech32PrefixForAccount("gonka", "gonka")

	escrow := types.SubnetEscrow{
		Id:      1,
		Creator: "gonka1creator",
		Slots:   make([]string, keeper.SubnetGroupSize),
	}
	_, err := k.StoreSubnetEscrow(ctx, &escrow)
	require.NoError(t, err)

	_, err = ms.SettleSubnetEscrow(ctx, &types.MsgSettleSubnetEscrow{
		Settler:  "gonka1wrong",
		EscrowId: 1,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "not the escrow creator")
}
