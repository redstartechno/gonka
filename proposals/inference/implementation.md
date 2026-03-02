# Inference Subnet: Implementation Plan

Phased implementation of the subnet described in [README.md](./README.md) and [design.md](./design.md). Each phase is self-contained: define scope, write tests first, implement, verify. Later phases build on earlier ones.

General approach: plan one phase at a time, implement, then plan the next. Test design is the priority -- tests define the contract before any implementation exists.

Test levels:
- Unit tests: per-package, in-process, no I/O
- Subnet integration tests: multi-node, mock MainnetBridge, stub InferenceEngine/ValidationEngine
- Testermint: full system (chain + dapi + subnet + mock ML nodes)

| Phase | Unit | Subnet integration | Testermint |
|-------|------|--------------------|------------|
| 1     | x    |                    |            |
| 2     | x    | in-process         |            |
| 3     | x    | multi-node (HTTP)  |            |
| 4     | x    | multi-node         |            |
| 5     | x    | + real chain query |            |
| 6     | x    | dapi-hosted        |            |
| 7     |      | user lib           |            |
| 8     | x    |                    |            |
| 9     |      |                    | x          |


## Phase 1: Foundation and State Machine

Goal: standalone `subnet/` Go module that can apply diffs, track state, compute hashes, and verify signatures. No networking, no mainnet integration, no gossip. Everything runs in-process, driven by Go tests.

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
  engine.go                 # InferenceEngine, ValidationEngine interfaces (stubs for phase 1)
  types.go                  # ExecuteRequest, ExecuteResult, ValidateRequest, ValidateResult (stubs)
  proto/
    subnet/v1/
      tx.proto              # 7 subnet tx message definitions + TimeoutVote
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

`proto/subnet/v1/tx.proto` -- all 7 subnet transaction types plus TimeoutVote:

```protobuf
syntax = "proto3";
package subnet.v1;
option go_package = "subnet/types";

message MsgStartInference {
  uint64 inference_id = 1;
  bytes  prompt_hash = 2;
  string model = 3;
  uint64 input_length = 4;          // prompt length in characters
  uint64 max_tokens = 5;            // max output tokens (matches request body)
  int64  started_at = 6;            // user's timestamp
  bytes  executor_sig = 7;          // optional: receipt from executor, skips pending if present
}

message MsgConfirmStart {
  uint64 inference_id = 1;
  bytes  executor_sig = 2;       // receipt: sign(inference_id || prompt_hash || model || input_length || max_tokens || started_at)
}

message MsgFinishInference {
  uint64 inference_id = 1;
  bytes  response_hash = 2;
  uint64 input_tokens = 3;          // actual input tokens from execution
  uint64 output_tokens = 4;         // actual output tokens from execution
  uint32 executor_slot = 5;
  bytes  proposer_sig = 6;          // host signature, verified on apply then discarded
}

message MsgTimeoutInference {
  uint64 inference_id = 1;
  string reason = 2;             // "refused" or "execution"
  repeated TimeoutVote votes = 3;
}

message TimeoutVote {
  uint32 voter_slot = 1;
  bool   accept = 2;
  bytes  signature = 3;          // sign(escrow_id || inference_id || reason || accept)
}

message MsgValidation {
  uint64 inference_id = 1;
  uint32 validator_slot = 2;
  bool   valid = 3;
  bytes  proposer_sig = 4;
}

message MsgValidationVote {
  uint64 inference_id = 1;
  uint32 voter_slot = 2;
  bool   vote_valid = 3;
  bytes  proposer_sig = 4;
}

message MsgRevealSeed {
  uint32 slot_id = 1;
  bytes  signature = 2;          // sign(escrow_id_bytes) with host's key
  bytes  proposer_sig = 3;
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
  uint64 input_length = 6;          // chars, from MsgStartInference
  uint64 max_tokens = 7;            // from MsgStartInference
  uint64 input_tokens = 8;          // actual, from MsgFinishInference
  uint64 output_tokens = 9;         // actual, from MsgFinishInference
  uint64 reserved_cost = 10;        // computed at start from (input_length, max_tokens, config)
  uint64 actual_cost = 11;          // computed at finish from (input_tokens, output_tokens, config)
  int64  started_at = 12;
  uint32 votes_valid = 13;
  uint32 votes_invalid = 14;
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
  StatusPending     InferenceStatus = iota
  StatusStarted
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
  InputLength   uint64          // prompt length in chars (from MsgStartInference)
  MaxTokens     uint64          // max output tokens (from MsgStartInference)
  InputTokens   uint64          // actual input tokens (set on MsgFinishInference)
  OutputTokens  uint64          // actual output tokens (set on MsgFinishInference)
  ReservedCost  uint64          // computed at start: f(input_length, max_tokens, config)
  ActualCost    uint64          // computed at finish: f(input_tokens, output_tokens, config)
  StartedAt     int64
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

type SessionConfig struct {
  DeadlineDuration int64   // seconds: deadline = started_at + DeadlineDuration
  TokenPrice       uint64  // price per token (input and output, same for now)
  CharsPerToken    uint64  // input_length / CharsPerToken = estimated input tokens
  VoteThreshold    uint32  // minimum slot-weighted accept votes for timeout
  GroupSize        uint32  // number of slots in the group
}

type SessionState struct {
  EscrowID    string
  Config      SessionConfig
  Balance     uint64
  Inferences  map[uint64]*InferenceRecord  // keyed by inference_id
  HostStats   map[uint32]*HostStats         // keyed by slot_id
  LatestNonce uint64
}

type SubnetTx struct {
  // one-of: each tx type
  StartInference   *MsgStartInference
  ConfirmStart     *MsgConfirmStart
  FinishInference  *MsgFinishInference
  Validation       *MsgValidation
  ValidationVote   *MsgValidationVote
  TimeoutInference *MsgTimeoutInference
  RevealSeed       *MsgRevealSeed
}

type Diff struct {
  Nonce      uint64
  Txs        []SubnetTx
  UserSig    []byte               // user signature over hash(serialize(Diff))
  Signatures map[uint32][]byte    // slot_id -> state signature (accumulated over time)
  StateHash  []byte               // computed after applying
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
2. For each tx in diff: validate well-formed, check preconditions, apply to SessionState
3. Compute state_root (two-level Merkle)
4. Return state_root

State transitions follow design.md "Inference Lifecycle":
- MsgStartInference: creates record status=pending (or started if executor_sig present). Computes reserved_cost from (input_length, max_tokens, config), reserves from balance. Deadline = started_at + config.DeadlineDuration (not stored in tx, derived).
- MsgConfirmStart: verifies executor receipt signature, pending->started
- MsgFinishInference: started->finished. Computes actual_cost from (input_tokens, output_tokens, config). Releases reserved_cost - actual_cost to balance. Updates host_stats[executor].cost += actual_cost.
- MsgValidation(valid=true): finished->validated
- MsgValidation(valid=false): finished->challenged
- MsgValidationVote: increments vote counts, resolves to validated or invalidated on majority. On invalidation: host_stats[executor].cost -= actual_cost, actual_cost returned to balance.
- MsgTimeoutInference: verifies vote signatures and slot-weighted threshold (config.VoteThreshold). pending/started->timed_out. host_stats[executor].missed += 1, reserved_cost released to balance.

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

State sign message format from design.md: `sha256(state_root || escrow_id_bytes || nonce_bytes)`.

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
  - Apply MsgStartInference, verify record created with status=pending, balance decremented by reserved_cost (derived from input_length + max_tokens + config)

TestApplyDiff_ConfirmStart
  - Apply Start then ConfirmStart with valid executor receipt, verify status=started

TestApplyDiff_ConfirmStart_InvalidReceipt
  - Apply Start then ConfirmStart with bad executor_sig, verify rejection

TestApplyDiff_StartInference_FastPath
  - Apply MsgStartInference with executor_sig present, verify status=started immediately (skips pending)

TestApplyDiff_FinishInference
  - Start -> ConfirmStart -> Finish, verify status=finished, balance += reserved_cost - actual_cost, host_stats.cost updated

TestApplyDiff_Validation_Valid
  - Start -> ConfirmStart -> Finish -> Validation(valid=true), verify status=validated

TestApplyDiff_Validation_Invalid_ChallengeVoting
  - Start -> ConfirmStart -> Finish -> Validation(valid=false), verify status=challenged
  - Apply votes, verify transition to validated or invalidated based on majority
  - Verify host_stats.invalid incremented on invalidation
  - Verify cost refunded on invalidation (host_stats.cost decremented, balance restored)

TestApplyDiff_Timeout_Refused
  - Start (pending, no receipt) -> Timeout(reason=refused, with accept votes)
  - Verify status=timed_out, host_stats.missed += 1, reserved_cost released to balance

TestApplyDiff_Timeout_Execution
  - Start -> ConfirmStart (started) -> Timeout(reason=execution, with accept votes)
  - Verify status=timed_out, host_stats.missed += 1, reserved cost released

TestApplyDiff_Timeout_InsufficientVotes
  - Timeout with too few accept votes, verify rejection

TestApplyDiff_Timeout_AfterFinish
  - Start -> ConfirmStart -> Finish -> Timeout, verify rejection (finished cannot transition to timed_out)

TestApplyDiff_NonceSequential
  - Apply diff with wrong nonce, verify error

TestApplyDiff_DuplicateTimeout
  - Two timeouts for same inference_id, second is ignored (already timed_out)

TestApplyDiff_FullLifecycle
  - 10 inferences through various paths (some finished, some timed_out, some validated)
  - Verify final host_stats match expectations
  - Verify final balance = escrow_amount - sum(host_stats[*].cost)

TestApplyDiff_EscrowBalanceCheck
  - Start inference when balance too low (available < reserved_cost), verify rejection
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
  - Each diff: MsgStartInference + accumulated MsgConfirmStart/MsgFinishInference from previous
  - Sign each state root with the receiving host's key
  - Verify 2/3+ signatures exist for the final state
  - Verify host_stats reflect correct cost distribution
  - Compute settlement data (state_root, rest_hash, host_stats, signatures)
```

### 1.9 Scope Boundaries

Phase 1 does NOT include:
- Networking, HTTP handlers, gossip
- MainnetBridge implementation (interface only)
- InferenceEngine/ValidationEngine implementation (interface stubs only)
- Round-robin enforcement (host role logic)
- Inclusion enforcement (K-round grace period)
- Timeout verification flow (vote collection from hosts)
- Host/User role separation
- Equivocation detection
- Validation scheduling (ShouldValidate, seed reveal)
- Real storage (SQLite/PostgreSQL)
- Proto generation CI pipeline (manual protoc for now)

These belong to later phases.


## Phase 2: Host and User Roles

Goal: protocol logic for both participants. Full session runnable in-process without networking.

Scope:
- Host role (execution loop):
  - Validate incoming diffs (nonce, signatures, well-formedness)
  - Execute inference when assigned as executor (via stub InferenceEngine)
  - Sign executor receipt (MsgConfirmStart data)
  - Sign state root (or withhold based on acceptance rules)
  - Propose MsgFinishInference after execution completes
  - Mempool management: track own proposed txs, include in response
  - Inclusion enforcement: withhold signature if mempool txs not included after K rounds
- User role:
  - Compose diffs with correct tx ordering
  - Round-robin host selection: `slot_at_position(nonce % group_size)`
  - MsgConfirmStart pipelining (receipt from host response -> included in next diff)
  - Signature collection and tracking
- InferenceEngine/ValidationEngine: stub implementations returning fixed results

Host responsibilities deferred to later phases:
- Validation (ShouldValidate, MsgValidation, MsgValidationVote) -> Phase 4
- Timeout verification participation (vote on timeout requests) -> Phase 3
- Gossip (nonce propagation, lazy tx gossip) -> Phase 3
- Seed reveal (MsgRevealSeed) -> Phase 4

Tests: in-process integration test with 5 hosts + 1 user. Full happy-path session (3 rounds, 15 inferences). Timeout detection (user-side). Signature withholding when mempool txs are not included. Inclusion enforcement unblocks when missing txs are added.

Testable deliverable: user composes correct diffs in round-robin order, hosts execute and propose MsgFinishInference, signatures accumulate to 2/3+, session completes with correct host_stats.


## Phase 3: Networking and Gossip

Goal: real HTTP transport between nodes. Gossip protocol. Timeout verification flow.

Scope:
- HTTP handlers for all subnet endpoints (see design.md API Surface):
  - POST /subnet/v1/sessions/{id}/chat/completions (inference with diffs)
  - POST /subnet/v1/sessions/{id}/verify-timeout (timeout verification)
  - POST /subnet/v1/sessions/{id}/gossip/nonce (nonce propagation)
  - POST /subnet/v1/sessions/{id}/gossip/txs (lazy tx gossip)
  - GET /subnet/v1/sessions/{id}/diffs (state recovery)
  - GET /subnet/v1/sessions/{id}/mempool (unsettled txs)
- Request authentication: X-Subnet-Signature header (see design.md Request Authentication)
- Gossip: nonce propagation to K=3 random peers, lazy tx gossip after K rounds, re-propagation on gap detection (120s)
- Timeout verification: user contacts non-executor hosts, hosts contact executor, return signed votes
- Equivocation detection: conflicting state hashes at same nonce via gossip

Multi-node test infrastructure: nodes as goroutines with real HTTP listeners on localhost. Mock MainnetBridge, stub InferenceEngine.

Tests: multi-node integration tests. Happy path over HTTP. Host-down + timeout verification (reason=refused and reason=execution). Equivocation detection and session termination. Lazy tx gossip triggers when user withholds host txs.

Testable deliverable: subnet cluster of N nodes communicates over HTTP, gossip detects gaps and equivocation, timeout verification works end-to-end.


## Phase 4: Validation and Settlement

Goal: probabilistic validation protocol, seed reveal, settlement data construction.

Scope:
- ShouldValidate logic: deterministic selection based on seed + inference_id
- Seed derivation: `first_8_bytes(sign(escrow_id_bytes))` per host per session
- MsgRevealSeed handling: verify seed signature against pinned signing key
- Two finalizing rounds before settlement (round 1: collect seeds + remaining txs; round 2: propagate complete state, hosts sign final state)
- Compliance computation: required_validations and completed_validations from revealed seeds + existing MsgValidation txs
- MsgSettleEscrow data construction: (state_root, rest_hash, host_stats, signatures)
- Dispute detection: compare proposed nonce against local latest_nonce

Relevant existing code in decentralized-api:
- `inference-chain/x/inference/calculations/should_validate.go`: `ShouldValidate()` and `DeterministicFloat()` are pure functions (~70 lines). Decide: reuse (requires importing chain module) or reimplement (avoids dependency).
- `decentralized-api/internal/seed/seed.go`: `CreateSeedForEpoch()` signs bytes and takes first 8 bytes -- same pattern as subnet seed derivation (signs escrow_id instead of epoch index).
- `decentralized-api/internal/validation/inference_validation.go`: `compareLogits()`, `customSimilarity()`, `customDistance()` are pure functions. Needed for ValidationEngine adapter in Phase 6, not here.

Decision point: ShouldValidate and seed derivation are small enough (~70 lines total) to reimplement in the subnet module. This avoids importing chain types and keeps the module boundary clean. compareLogits stays in dapi (used by the ValidationEngine adapter in Phase 6).

Tests: full session with validation, seed reveal, finalizing rounds, settlement data verified. Both in-process and multi-node.

Testable deliverable: session ends with correct settlement payload. Seed reveal produces valid ShouldValidate expectations. Compliance numbers match. Settlement Merkle proof is verifiable.


## Phase 5: Chain Adapter (MainnetBridge)

Goal: real mainnet communication via REST. Host discovery.

Scope:
- MainnetBridge implementation via chain's grpc-gateway REST endpoints
- GetEscrow, GetValidatorInfo, VerifyWarmKey (authz grant check)
- Host discovery: /v1/identity endpoint, DelegateTAs, deterministic instance selection via `hash(escrow_id, app_hash) % len(DelegateTAs)`
- Slot assignment derivation from (app_hash, escrow_id, validator_weights) using existing `GetSlotsFromSorted`
- Notification handlers: OnEscrowCreated, OnSettlementProposed, OnSettlementFinalized
- Event subscription: listen for escrow creation and settlement events from chain

Tests: integration tests against testnet chain for real data fetching. Mock bridge preserved for all prior test levels. Warm key verification tested with real authz grants. Slot assignment verified against chain computation.

Testable deliverable: bridge adapter correctly fetches escrow info, validator data, verifies warm keys from a running chain. Slot assignment derivation matches chain-side result.


## Phase 6: dapi Integration and ML Node Adapters

Goal: subnet runs as part of decentralized-api. Real ML node interaction via existing infrastructure.

Scope:
- InferenceEngine adapter wrapping broker + completionapi (execute inference on vLLM node, stream response, extract hashes and token counts)
- ValidationEngine adapter wrapping broker + validation logic (re-execute with enforced tokens, compareLogits)
- Router: mount `/subnet/v1/` echo group on existing dapi public server
- MainnetBridge adapter using dapi's chain client (alternative to REST bridge from Phase 5)
- SQLite storage (WAL mode, single writer goroutine, as specified in design.md)

Tests: subnet running inside dapi, mock ML node (WireMock), real signing, real SQLite, real gossip over localhost. Full session with actual inference execution and streaming.

Testable deliverable: dapi serves subnet inference requests, executes on ML node via existing broker, returns streaming results with subnet protocol (diffs, signatures, mempool).


## Phase 7: User Client Library

Goal: Go library for the user side of the protocol. Callable from code, usable in integration tests and testermint.

Scope:
- User SDK: open session (escrow_id, bridge), send inference requests (OpenAI-compatible), handle receipts and pipelining, collect signatures, trigger finalizing rounds, construct settlement payload
- Go library API: no HTTP server, direct function calls
- Later iteration: proxy mode wrapping the library behind an OpenAI-compatible HTTP server (user sends normal /chat/completions, proxy handles all subnet protocol transparently)

Tests: Go test creates user client, sends inference requests to a subnet cluster, gets streaming responses, verifies session settles correctly.

Testable deliverable: user client library drives a full session against real subnet nodes. No manual diff construction or protocol handling from test code.


## Phase 8: Mainnet Modifications

Goal: MsgCreateEscrow and MsgSettleEscrow in the inference chain module.

Scope:
- Proto definitions in `inference-chain/proto/` for MsgCreateEscrow and MsgSettleEscrow
- Keeper logic: escrow creation (lock funds, store escrow info, record app_hash), settlement verification (recompute host_stats_hash, verify Merkle proof `hash(host_stats_hash || rest_hash) == state_root`, check 2/3+ slot-weighted signatures)
- Escrow distribution: pay each host from host_stats[slot].cost, refund remaining balance to user
- Record host_stats on chain (for reputation, future punishment logic)
- Dispute window: X blocks after settlement proposal. Competing state with higher nonce and valid signatures overrides.
- Host-initiated settlement after timeout (escrow expiry height or wall-clock from last nonce)

Tests: keeper unit tests in `inference-chain/x/inference/keeper/`. Settlement verification using test vectors from Phase 4 (known state_root, host_stats, signatures from subnet integration tests).

Testable deliverable: chain correctly creates escrows, verifies settlement Merkle proofs and signatures, distributes funds, handles disputes.


## Phase 9: End-to-End (Testermint)

Goal: full system tested through testermint integration framework.

Scope:
- Testermint test: MsgCreateEscrow -> inference requests via user client library -> MsgSettleEscrow
- Verify mainnet state after settlement: host balances increased, user refund correct, host_stats recorded
- Edge cases: host down during session (timeout recovery), user disappears (host-initiated settlement)
- Verify integration with existing epoch/PoC system (subnet sessions coexist with current inference flow)

Tests: testermint integration tests using user client library from Phase 7 against full local cluster (chain nodes + dapi + mock ML nodes).

Testable deliverable: complete system works end-to-end. Settlement produces correct on-chain state.
