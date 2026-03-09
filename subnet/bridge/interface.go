package bridge

// MainnetBridge defines the interface between the subnet and mainnet.
// Phase 1: interface only, no implementation.
type MainnetBridge interface {
	// Notifications: mainnet -> subnet
	OnEscrowCreated(escrow EscrowInfo) error
	OnSettlementProposed(escrowID string, stateRoot []byte, nonce uint64) error
	OnSettlementFinalized(escrowID string) error

	// Queries: subnet -> mainnet
	GetEscrow(escrowID string) (*EscrowInfo, error)
	GetValidatorInfo(validatorAddress string) (*ValidatorInfo, error)
	VerifyWarmKey(warmAddress, validatorAddress string) (bool, error)

	// Actions: subnet -> mainnet
	SubmitDisputeState(escrowID string, stateRoot []byte, nonce uint64, sigs map[uint32][]byte) error
}

type EscrowInfo struct {
	EscrowID       string
	Amount         uint64
	CreatorAddress string
	AppHash []byte
	Slots          []string // validator addresses, len == SubnetGroupSize
}

type ValidatorInfo struct {
	Address   string
	PublicKey []byte
	URL       string
	Weight    uint64
}
