# Inference Subnet: Implementation Plan

Phased implementation of the subnet described in [README.md](./README.md) and [design.md](./design.md). Each phase is self-contained: define scope, write tests first, implement, verify. Later phases build on earlier ones.

General approach: plan one phase at a time, implement, then plan the next. Test design is the priority -- tests define the contract before any implementation exists.


## Phase 1: State Machine Core

Goal: a standalone `subnet/` Go module that can apply diffs, track state, compute hashes, and verify signatures. No networking, no mainnet integration, no gossip. Everything runs in-process, driven by Go tests.

Deliverables:
1. Project structure and Go module
2. Proto definitions for all 7 subnet transaction types
3. Domain types (SessionState, InferenceRecord, HostStats, Diff)
4. State machine: apply diffs, verify nonces, update balances/stats
5. State hashing (two-level Merkle)
6. Signing: secp256k1 sign/verify via go-ethereum/crypto
7. Storage interface + in-memory implementation
8. Tests covering the full inference lifecycle

### 1.1 Project and Module Setup

Create `subnet/` at repo root with its own `go.mod`. Dependencies: `go-ethereum/crypto` (secp256k1), `google.golang.org/protobuf` (proto). No cosmos-sdk.

```
subnet/
  go.mod
  engine.go                 # InferenceEngine, ValidationEngine interfaces (empty, phase 1 stubs)
  types.go                  # ExecuteRequest, ExecuteResult, ValidateRequest, ValidateResult (empty stubs)
  proto/
    subnet/v1/
      tx.proto              # 7 subnet tx message definitions
      state.proto           # SessionState, InferenceRecord, HostStats for deterministic serialization
  types/
    generated.go            # generated proto types
    domain.go               # Diff, SlotAssignment, non-proto domain types
  state/
    interface.go            # StateMachine interface
    machine.go              # implementation: ApplyDiff, ComputeStateRoot
    hash.go                 # two-level Merkle hash computation
    machine_test.go         # tests
  signing/
    interface.go            # Signer, Verifier interfaces
    secp256k1.go            # go-ethereum/crypto implementation
    secp256k1_test.go       # tests
  storage/
    interface.go            # Storage interface (5 methods from design.md)
    memory.go               # in-memory implementation
    memory_test.go          # tests
  bridge/
    interface.go            # MainnetBridge interface (7 methods, no implementation in phase 1)
  gossip/
    interface.go            # GossipClient interface (empty, no implementation in phase 1)
```

### 1.2 Proto Definitions

`proto/subnet/v1/tx.proto` -- all 7 subnet transaction types:

```protobuf
syntax = "proto3";
package subnet.v1;
option go_package = "subnet/types";

message MsgStartInference {
  uint64 inference_id = 1;
  bytes  prompt_hash = 2;
  string model = 3;
  uint64 estimated_cost = 4;
  int64  started_at = 5;        // unix timestamp
  int64  deadline = 6;          // started_at + T
}

message MsgFinishInference {
  uint64 inference_id = 1;
  bytes  response_hash = 2;
  uint64 input_tokens = 3;
  uint64 output_tokens = 4;
  uint64 actual_cost = 5;
  uint32 executor_slot = 6;
}

message MsgValidation {
  uint64 inference_id = 1;
  uint32 validator_slot = 2;
  bool   valid = 3;
}

message MsgValidationVote {
  uint64 inference_id = 1;
  uint32 voter_slot = 2;
  bool   vote_valid = 3;
}

message MsgTimeoutInference {
  uint64 inference_id = 1;
  int64  timestamp = 2;
  uint32 proposer_slot = 3;
}

message MsgRequestPrompt {
  uint64 inference_id = 1;
  uint32 requester_slot = 2;
  int64  target_height = 3;      // mainnet block height for relay group sampling
}

message MsgRevealSeed {
  uint32 slot_id = 1;
  bytes  signature = 2;          // sign(escrow_id_bytes) with host's key
}
```

`proto/subnet/v1/state.proto` -- for deterministic serialization in hash computation:

```protobuf
syntax = "proto3";
package subnet.v1;
option go_package = "subnet/types";

// Used for deterministic serialization when computing state hashes.
// Not the runtime state representation (that's domain types in Go).

message HostStatsProto {
  uint32 slot_id = 1;
  uint32 missed = 2;
  uint32 invalid = 3;
  uint64 cost = 4;
  uint32 required_validations = 5;
  uint32 completed_validations = 6;
}

message HostStatsMapProto {
  repeated HostStatsProto entries = 1;  // sorted by slot_id
}

message InferenceRecordProto {
  uint64 inference_id = 1;
  uint32 status = 2;
  uint32 executor_slot = 3;
  bytes  prompt_hash = 4;
  bytes  response_hash = 5;
  uint64 input_tokens = 6;
  uint64 output_tokens = 7;
  uint64 cost = 8;
  int64  started_at = 9;
  int64  deadline = 10;
  uint32 votes_valid = 11;
  uint32 votes_invalid = 12;
}

message InferencesMapProto {
  repeated InferenceRecordProto entries = 1;  // sorted by inference_id
}
```

Generate Go code from protos using `protoc` (not ignite -- subnet is standalone).

### 1.3 Domain Types

`types/domain.go`:

```go
type InferenceStatus uint8

const (
  StatusStarted     InferenceStatus = iota
  StatusFinished
  StatusChallenged
  StatusValidated
  StatusInvalidated
  StatusTimedOut
)

type InferenceRecord struct {
  Status        InferenceStatus
  ExecutorSlot  uint32
  PromptHash    []byte
  ResponseHash  []byte
  InputTokens   uint64
  OutputTokens  uint64
  Cost          uint64
  StartedAt     int64
  Deadline      int64
  VotesValid    uint32
  VotesInvalid  uint32
  VotedSlots    map[uint32]bool
}

type HostStats struct {
  Missed               uint32
  Invalid              uint32
  Cost                 uint64
  RequiredValidations  uint32
  CompletedValidations uint32
}

type SessionState struct {
  EscrowID    string
  Balance     uint64
  Inferences  map[uint64]*InferenceRecord  // keyed by inference_id
  HostStats   map[uint32]*HostStats         // keyed by slot_id
  LatestNonce uint64
}

type SubnetTx struct {
  // one-of: each tx type
  StartInference   *MsgStartInference
  FinishInference  *MsgFinishInference
  Validation       *MsgValidation
  ValidationVote   *MsgValidationVote
  TimeoutInference *MsgTimeoutInference
  RequestPrompt    *MsgRequestPrompt
  RevealSeed       *MsgRevealSeed
}

type Diff struct {
  Nonce      uint64
  Txs        []SubnetTx
  Signatures map[uint32][]byte  // slot_id -> signature (accumulated)
  StateHash  []byte             // computed after applying
  CreatedAt  int64
}

type SlotAssignment struct {
  SlotID           uint32
  ValidatorAddress string
  PublicKey        []byte
  Weight           uint64
}
```

### 1.4 State Machine

`state/interface.go`:

```go
type StateMachine interface {
  // ApplyDiff validates and applies a diff at the next expected nonce.
  // Returns the computed state root after application.
  ApplyDiff(diff Diff) (stateRoot []byte, err error)

  // GetState returns a snapshot of the current session state.
  GetState() SessionState

  // ComputeStateRoot returns the current state root without modifying state.
  ComputeStateRoot() []byte
}
```

Diff application follows design.md section "Diff Application":
1. Validate nonce is sequential (latest_nonce + 1)
2. For each tx in diff: validate well-formed, apply to SessionState
3. Check active inferences for timeout (compare deadline against provided timestamp)
4. Compute state_root (two-level Merkle)
5. Return state_root

State transitions follow design.md "Inference Lifecycle" exactly.

### 1.5 State Hashing

Two-level Merkle from design.md:

```
         state_root
        /          \
host_stats_hash    rest_hash
```

- host_stats_hash = sha256(proto_serialize(HostStatsMapProto{sorted by slot_id}))
- inferences_hash = sha256(proto_serialize(InferencesMapProto{sorted by inference_id}))
- rest_hash = sha256(balance_bytes || inferences_hash)
- state_root = sha256(host_stats_hash || rest_hash)

Proto serialization ensures determinism (sorted keys, fixed field order).

### 1.6 Signing

`signing/interface.go`:

```go
type Signer interface {
  // Sign signs the message and returns the signature.
  Sign(message []byte) ([]byte, error)

  // Address returns the signer's address derived from its public key.
  Address() string
}

type Verifier interface {
  // RecoverAddress recovers the signer's address from message and signature.
  RecoverAddress(message []byte, signature []byte) (string, error)
}
```

What gets signed: `sha256(state_root || escrow_id_bytes || nonce_bytes)`. This is the sign message format from design.md.

### 1.7 Storage

Reuse interface from design.md:

```go
type Storage interface {
  CreateSession(escrowID string, group []SlotAssignment, balance uint64) error
  AppendDiff(escrowID string, diff Diff) error
  AddSignature(escrowID string, nonce uint64, slotID uint32, sig []byte) error
  GetState(escrowID string) (*EscrowState, error)
  GetDiffs(escrowID string, fromNonce, toNonce uint64) ([]Diff, error)
}
```

Phase 1 implementation: in-memory map protected by mutex.

### 1.8 Test Plan

Tests are the primary deliverable. They define the contract and validate the state machine before anything else exists.

**State machine tests** (`state/machine_test.go`):

```
TestApplyDiff_StartInference
  - Apply MsgStartInference, verify record created with status=started, balance decremented

TestApplyDiff_FinishInference
  - Apply Start then Finish, verify status=finished, cost adjusted, host_stats.cost updated

TestApplyDiff_Validation_Valid
  - Start -> Finish -> Validation(valid=true), verify status=validated

TestApplyDiff_Validation_Invalid_ChallengeVoting
  - Start -> Finish -> Validation(valid=false), verify status=challenged
  - Apply votes, verify transition to validated or invalidated based on majority
  - Verify host_stats.invalid incremented on invalidation
  - Verify balance refunded on invalidation

TestApplyDiff_Timeout
  - Start -> Timeout (after deadline), verify status=timed_out
  - Verify host_stats.missed incremented, balance refunded

TestApplyDiff_Timeout_BeforeDeadline
  - Start -> Timeout (before deadline), verify rejection

TestApplyDiff_NonceSequential
  - Apply diff with wrong nonce, verify error

TestApplyDiff_DuplicateTimeout
  - Two timeouts for same inference_id, second is ignored

TestApplyDiff_FullLifecycle
  - 10 inferences through various paths, verify final host_stats match expectations

TestApplyDiff_EscrowBalanceCheck
  - Start inference when balance too low, verify rejection
```

**State hash tests** (`state/hash_test.go`):

```
TestComputeStateRoot_Deterministic
  - Same state produces same root across multiple calls

TestComputeStateRoot_DifferentState
  - Different balance produces different root

TestStateRoot_MerkleStructure
  - Verify hash(host_stats_hash || rest_hash) == state_root
  - Verify rest_hash = hash(balance_bytes || inferences_hash)

TestStateRoot_SortedKeys
  - host_stats with slot_ids [5, 2, 8] produces same hash regardless of insertion order
```

**Signing tests** (`signing/secp256k1_test.go`):

```
TestSign_RecoverAddress
  - Sign message, recover address, verify match

TestSign_DifferentKeys
  - Two keys produce different signatures, recover different addresses

TestVerify_TamperedMessage
  - Modify message after signing, verify recovered address differs
```

**Storage tests** (`storage/memory_test.go`):

```
TestCreateSession_GetState
  - Create, retrieve, verify fields

TestAppendDiff_GetDiffs
  - Append 5 diffs, retrieve range, verify order and content

TestAddSignature
  - Append diff, add signature later, verify it appears in GetDiffs result
```

**Integration test** (`state/integration_test.go`):

```
TestFullSession_HappyPath
  - Create state machine with 5-slot group
  - Apply 15 diffs (3 full rounds of round-robin)
  - Each diff: MsgStartInference + accumulated MsgFinishInference from previous
  - Sign each state root with the receiving host's key
  - Verify 2/3+ signatures exist for the final state
  - Verify host_stats reflect correct cost distribution
  - Compute settlement data (state_root, rest_hash, host_stats, signatures)
```

### 1.8 Scope boundaries

Phase 1 does NOT include:
- Networking, HTTP handlers, gossip
- MainnetBridge implementation (interface only)
- InferenceEngine/ValidationEngine implementation (interface stubs only)
- Round-robin enforcement (host role logic)
- Inclusion enforcement (K-round grace period)
- Recovery protocol (MsgRequestPrompt flow)
- Host/User role separation
- Real storage (SQLite/PostgreSQL)
- Proto generation CI pipeline (manual protoc for now)

These belong to later phases.


## Phase 2: Host and User Roles

TODO -- plan after Phase 1 is implemented.

Likely scope: host request handling (validate incoming diffs, sign state, propose MsgFinishInference), user sequencing (round-robin, signature collection, diff composition), inclusion enforcement.


## Phase 3: Gossip and Recovery

TODO -- plan after Phase 2.

Likely scope: nonce propagation, lazy tx gossip, MsgRequestPrompt flow, re-propagation.


## Phase 4: dapi Integration

TODO -- plan after Phase 3.

Likely scope: engine adapters, router mounting, real MainnetBridge implementation, SQLite storage.


## Phase 5: Settlement and Dispute

TODO -- plan after Phase 4.

Likely scope: MsgSettleEscrow on mainnet, dispute window, host-initiated settlement, finalizing round orchestration.
