package types

// InferenceStatus represents the lifecycle state of an inference.
type InferenceStatus uint8

const (
	StatusPending     InferenceStatus = iota
	StatusStarted
	StatusFinished
	StatusChallenged
	StatusValidated
	StatusInvalidated
	StatusTimedOut
)

// InferenceRecord tracks the state of a single inference within a session.
type InferenceRecord struct {
	Status       InferenceStatus
	ExecutorSlot uint32
	Model        string
	PromptHash   []byte
	ResponseHash []byte
	InputLength  uint64
	MaxTokens    uint64
	InputTokens  uint64
	OutputTokens uint64
	ReservedCost uint64
	ActualCost   uint64
	StartedAt    int64
	ConfirmedAt  int64
	VotesValid     uint32
	VotesInvalid   uint32
	VotedSlots     Bitmap128
	ValidatorSlot  uint32
	ValidatorValid bool
	ValidatedBy    Bitmap128
}

// HostStats tracks per-host performance metrics within a session.
type HostStats struct {
	Missed               uint32
	Invalid              uint32
	Cost                 uint64
	RequiredValidations  uint32
	CompletedValidations uint32
}

// SessionConfig holds session-level parameters.
type SessionConfig struct {
	RefusalTimeout   int64  // seconds before reason=refused timeout
	ExecutionTimeout int64  // seconds before reason=execution timeout
	TokenPrice       uint64 // price per unit (flat per session)
	VoteThreshold    uint32 // minimum accept votes for timeout (total_slots / 2)
	ValidationRate   uint32 // basis points (10000 = 100%, 1000 = 10%)
}

// EscrowState is the full state of a subnet session.
type EscrowState struct {
	EscrowID    string
	Config      SessionConfig
	Group       []SlotAssignment
	Balance       uint64
	Finalizing    bool
	Inferences    map[uint64]*InferenceRecord
	HostStats     map[uint32]*HostStats
	RevealedSeeds map[uint32]int64
	LatestNonce   uint64
}

// Diff is the protocol primitive: what the user creates and signs.
// UserSig covers hash(proto_serialize(Nonce, Txs)).
// Txs uses the proto-generated SubnetTx with its oneof discriminator,
// which structurally guarantees exactly one tx type per entry.
type Diff struct {
	Nonce         uint64
	Txs           []*SubnetTx
	UserSig       []byte
	PostStateRoot []byte
}

// DiffRecord is the storage representation: Diff + computed metadata.
type DiffRecord struct {
	Diff
	StateHash  []byte
	Signatures map[uint32][]byte
	CreatedAt  int64
}

// SlotAssignment maps a slot to a validator in the session group.
type SlotAssignment struct {
	SlotID           uint32
	ValidatorAddress string
	PublicKey        []byte
	Weight           uint64
}
