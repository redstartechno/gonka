# Inference Subnet: Technical Design

Working document. Captures design decisions and open questions for the subnet implementation described in [README.md](./README.md).

## No Cosmos SDK in Subnet

The subnet does not depend on Cosmos SDK. Cosmos SDK is slow, heavyweight, and the subnet is not a blockchain.

Mainnet keeps Cosmos SDK for `MsgCreateEscrow` and `MsgSettleEscrow` in `inference-chain/x/inference/`.

Crypto: `go-ethereum/crypto` for secp256k1 signing/ecrecover, `crypto/sha256` for hashing. Proto definitions are self-contained within the subnet package.

Keys are secp256k1, same as mainnet. A host signs with its warm key (authz grant) or cold key (validator account key).


## Transaction List

### Mainnet (2 txs)

Defined in `inference-chain/proto/` alongside existing 43 tx types.

| Tx | Proposer | Purpose |
|----|----------|---------|
| MsgCreateEscrow | user | Lock funds, source of data for group sampling |
| MsgSettleEscrow | user or host | Finalize session, initiate escrow distribution |

### Subnet (7 txs)

Defined in the subnet package's own proto files. No shared types with mainnet protos.

| Tx | Proposer | Purpose |
|----|----------|---------|
| MsgStartInference | user | Authorize inference, reserve cost from escrow balance |
| MsgFinishInference | host | Record completion, response hash, token counts |
| MsgValidation | host | Validation result. valid=true -> validated. valid=false -> opens challenge voting |
| MsgValidationVote | host | Vote during challenge window (after MsgValidation valid=false) |
| MsgTimeoutInference | user | Declare inference timed out. First per inference_id wins, duplicates ignored |
| MsgRequestPrompt | host | Recovery: request prompt data the user withheld |
| MsgRevealSeed | host | Reveal validation seed during finalizing round |

9 total (2 mainnet + 7 subnet). No governance, staking, or other chain overhead in the subnet.

No separate `MsgInvalidateInference`. Invalidation is the result of a challenge voting round: `MsgValidation(valid=false)` opens the vote, `MsgValidationVote` collects votes, majority decides. This replaces the current mainnet pattern where `Validation` and `InvalidateInference` are separate RPCs.

MsgTimeoutInference is user-proposed. The user is the one waiting for the result and has direct incentive to declare timeout on non-responsive hosts. Defense against premature timeouts: hosts check their own clocks and refuse to sign if the deadline (started_at + T) hasn't passed. Dedup: content-addressed by inference_id, first one wins.


## Package Structure

Top-level Go module: `subnet/` at repo root. Imported by `decentralized-api` as a library.

### Both Roles in One Library

The library implements both the host and user flows. The state machine is shared -- diffs, signature verification, nonce tracking, state hashing. The difference is who proposes transactions and who sequences.

Host-specific: validate incoming requests, sign state, propose MsgFinishInference/MsgValidation, gossip nonces, enforce inclusion rules.

User-specific: create MsgStartInference, sequence diffs, round-robin host selection, collect signatures, submit settlement.

A single Go test can create 30 host nodes and 1 user node, drive a full session, and verify everything in-process. When we need a JS/Python client later, the Go library is the reference and the wire protocol is the contract.

```
subnet/
  go.mod                    # standalone module, deps: go-ethereum/crypto, protobuf. no cosmos-sdk
  engine.go                 # InferenceEngine, ValidationEngine interfaces (contract with dapi)
  types.go                  # ExecuteRequest, ExecuteResult, ValidateRequest, ValidateResult
  proto/                    # subnet-specific proto definitions
  types/                    # generated proto types + domain types
  state/                    # state machine: apply diffs, verify nonces, track balances
  host/                     # host role: request handling, signing, gossip, inclusion enforcement
  user/                     # user role: sequencing, round-robin, signature collection, settlement
  signing/                  # signature creation and verification
  storage/                  # storage interface + implementations
  gossip/                   # gossip client and handlers
  bridge/                   # MainnetBridge interface

decentralized-api/
  internal/
    subnet/
      engine_adapter.go     # implements subnet.InferenceEngine using broker + completionapi
      validation_adapter.go # implements subnet.ValidationEngine using broker + compareLogits
      router.go             # mounts /subnet/v1/ routes, wires adapters to subnet host
```

The engine interfaces at the subnet root are the contract between subnet and dapi. They define what the subnet needs from the ML node infrastructure without importing any dapi or cosmos-sdk types. dapi adapters implement them by wrapping existing broker, completionapi, and validation logic.

Module boundary enforces no cosmos-sdk at compile time. During development, dapi uses a replace directive:

```
// decentralized-api/go.mod
replace subnet => ../subnet
```

First release: `decentralized-api` imports `subnet/` and mounts a new echo router group on the existing public server port, e.g. `/subnet/v1/`. The user flow is available as a Go client library (`subnet/user`) for integration tests and future standalone client tooling.


## State Machine

### Session State

Current state after applying all diffs up to latest_nonce. History lives in diffs (storage).

```
SessionState:
  escrow_id            string
  balance              uint64                          # remaining escrow
  inferences           map[uint64]InferenceRecord      # keyed by inference_id
  host_stats           map[uint32]HostStats            # keyed by slot_id
  latest_nonce         uint64
```

```
InferenceRecord:
  status               enum {started, finished, challenged, validated, invalidated, timed_out}
  executor_slot        uint32
  prompt_hash          []byte
  response_hash        []byte              # set on MsgFinishInference
  input_tokens         uint64              # set on MsgFinishInference
  output_tokens        uint64              # set on MsgFinishInference
  cost                 uint64              # reserved on start, finalized on finish
  started_at           int64               # unix timestamp from MsgStartInference
  deadline             int64               # started_at + T seconds
  votes_valid          uint32              # count during challenge
  votes_invalid        uint32              # count during challenge
  voted_slots          map[uint32]bool     # prevent double vote
```

```
HostStats:
  missed               uint32
  invalid              uint32
  cost                 uint64              # total cost of inferences executed by this host
  required_validations uint32              # inferences ShouldValidate selected for this host
  completed_validations uint32             # MsgValidation txs actually submitted
  session_passed        bool
```

Signatures are not part of SessionState. They are stored alongside diffs in storage.

### Inference Lifecycle

```
started -> finished                                     (happy path, most common)
started -> finished -> validated                         (validator confirmed correct, no challenge)
started -> finished -> challenged -> validated           (challenge dismissed by majority vote)
started -> finished -> challenged -> invalidated         (challenge confirmed by majority vote)
started -> timed_out                                     (host never finished within deadline)
started -> timed_out -> finished                         (timeout overridden: host finished after timeout was recorded)
```

Validation is probabilistic. Most inferences are never validated -- `started -> finished` is the normal terminal path. When validation does occur, `MsgValidation(valid=true)` moves directly to validated without voting. Only `MsgValidation(valid=false)` opens a challenge round.

Transitions and state updates:

- MsgStartInference: creates record with status=started, reserves estimated cost from balance.
- MsgFinishInference: status=finished, records response_hash and actual token counts, finalizes cost (adjusts balance if actual differs from estimate). Updates host_stats[executor_slot].cost += finalized cost.
- MsgValidation(valid=true): status=validated. No change to host_stats.
- MsgValidation(valid=false): status=challenged, opens voting window.
- MsgValidationVote: increments votes_valid or votes_invalid. When votes_invalid > group_size/2: status=invalidated, host_stats[executor].invalid += 1, host_stats[executor].cost -= cost, balance += cost (user refund -- shouldn't pay for bad output). When votes_valid > group_size/2 or voting window expires: status=validated.
- MsgTimeoutInference: status=timed_out, host_stats[executor].missed += 1, host_stats[executor].cost -= cost, balance += cost (user refund). User-proposed. Whether the deadline (started_at + T, where T >= 20 minutes) has actually passed is a local acceptance decision by each participant (see Timestamp Validation). State machine applies it unconditionally; hosts that disagree refuse to sign.
- MsgFinishInference after timeout: if status is timed_out, MsgFinishInference overrides it. status=finished, host_stats[executor].missed -= 1, host_stats[executor].cost += cost, balance -= cost. Reverses the timeout effects. Inclusion enforcement ensures the user includes MsgFinishInference if the host produced one.

### State Hash

The state is structured as a two-level hash:

```
           state_root
          /          \
 host_stats_hash    rest_hash
```

host_stats_hash = hash(serialize(host_stats)). rest_hash = hash(balance_bytes || inferences_hash), where inferences_hash = hash(serialize(inferences)). state_root = hash(host_stats_hash || rest_hash). Serialization is deterministic (protobuf with sorted map keys, fixed field order).

This covers the full SessionState: host_stats on the left, balance and inferences on the right. escrow_id and latest_nonce are already bound by the signature (sign(state_root || escrow_id || nonce)) and don't need separate inclusion.

At settlement, mainnet receives host_stats and rest_hash (the sibling). It recomputes host_stats_hash, combines with rest_hash, and checks against the signed state_root. Mainnet doesn't need to interpret rest_hash.

Every host applying the same diffs to the same nonce produces the same state_root. This is what gets signed.

### What Gets Signed

User signs each diff: `sign(hash(serialize(Diff)))`. Diff is protobuf (canonical serialization). The signature authenticates the diff as coming from the user and binds the tx content to the nonce. Required for non-repudiation and equivocation detection (see Sequencing Model).

Host signs the state: `sign(state_root || escrow_id || nonce)`. escrow_id prevents cross-session replay, nonce prevents cross-nonce replay, state_root binds to a specific state. Signing happens before execution.

### Diff Application

When a host receives a request with diffs:

1. For each diff from local_latest_nonce+1 to received_latest_nonce:
   a. Verify user signature on the diff
   b. Validate: nonce is sequential, txs are well-formed, proposer is authorized
   c. Apply each tx to SessionState (update balance, inferences, host_stats, usage)
   d. Compute state_root, store diff + state_root
2. Verify included host signatures against stored state_roots at their respective nonces
3. Append new diff (current nonce) with the user's new txs
4. Acceptance check: if pending host txs are satisfied, sign state_root and return signature. Otherwise return rejection with missing txs. Both include host mempool.

If any diff fails validation (bad signature, bad nonce, malformed tx), reject the entire request.

### Timestamp Validation

Nonce is the subnet's block height -- the authoritative ordering. The state machine is deterministic: same diffs at same nonces produce the same state on every host.

Wall-clock time is a local observable, not consensus truth. Different hosts have different clocks. Time is what a host uses to form a local opinion, not a protocol-level rule.

Two layers:

State machine (deterministic): applies diffs and computes state. Given the same diffs at the same nonces, every host produces the same result. No local clocks involved.

Acceptance (local): each participant independently decides whether to sign at a given nonce. This covers transaction freshness (is this tx recent?) and inference deadline expiry (has the executor's time window passed?). A host checks its own clock, forms an opinion, and either signs or refuses. This is a local judgment, not a protocol constant.

Refusal does not prevent diff application -- the state machine always applies diffs to keep state in sync across all hosts.

A disagreement resolves in one of two ways:
- State self-corrects: e.g., a disputed timeout is overridden by MsgFinishInference in a later diff. The host evaluates the current state, sees it's acceptable, signs normally.
- Supermajority resolution: the state is not corrected, but 2/3+ slot-weighted signatures exist for some nonce >= the disputed one. The group accepted the state. The host treats the dispute as resolved by group consensus and signs future nonces normally. Same threshold as settlement. Analogous to blockchain finality -- once enough attestations exist, individual disagreements don't persist.

**Monotonicity.** All timestamps must be non-decreasing across nonces. This is a deterministic check on the diff chain -- every host verifies it independently. Diffs that violate monotonicity are rejected.

**Gossip on rejection.** A silent rejection is not enough. The proposer can skip the rejecting party and claim it was unavailable. When a participant rejects a transaction due to a suspicious timestamp, it gossips signed evidence (the transaction, its own clock reading) to the group. Each group member forms its own opinion based on its own clock. No single host's clock is authoritative. The group's collective behavior -- enough hosts refusing to sign or enough accepting -- determines the outcome.


## Sequencing Model

### Primitives

Log: ordered sequence of diffs, indexed by nonce. Single writer (user). Append-only.

Diff: list of txs at a nonce, signed by the user. Immutable once in the log.

State: deterministic function of log[1..N]. Hosts compute state from diffs.

> Q: Can we avoid maintaining a state machine on the user side? The user signs diffs, not state -- so it doesn't strictly need to compute state. But without local state the user can't independently verify which state_hash is correct (until it gets the one confirmed by supermajority). Running the state machine in the user client means every state machine update requires updating the user library.

Round: one pass through all hosts in the group in slot order.

### Tx Sources

User-proposed: MsgStartInference, MsgTimeoutInference.

Host-proposed: MsgFinishInference, MsgValidation, MsgValidationVote, MsgRequestPrompt, MsgRevealSeed. Returned in host response mempool. User includes in future diffs.

The user is the sequencer for both.

### Flow

The user composes diffs and sends requests in round-robin order. Each request carries all accumulated diffs since the receiving host's last sync point. The user signs each diff (see What Gets Signed). Within a round, the user sends to the next host without waiting for the previous host's response.

Each host on receiving a request:
1. Verify user signature on each new diff
2. Apply all diffs through the state machine (deterministic, always runs)
3. For its assigned nonce: acceptance layer decides whether to sign
4. Gossip (nonce, state_hash) to K peers
5. Return: host signature + mempool (accept) or missing txs + mempool (reject)

### Rejection

A host refuses to sign for two reasons:
- Inclusion: pending txs from its mempool are not included.
- Acceptance: a tx violates the host's local judgment (e.g., premature timeout).

The user cannot retry a past nonce -- subsequent hosts already built on it. The user includes missing txs in the next diff and continues. If signatures are missing after a round, one additional round resolves them. No special retry protocol.

### Equivocation

Hosts gossip (nonce, state_hash) after processing each request. If a host sees a different state_hash for the same nonce from another host, it requests the diff at that nonce. Two different user-signed diffs at the same nonce = equivocation proof. The detecting host gossips the evidence and stops signing. The session terminates; hosts settle at the last clean state via host-initiated settlement.

### Session Structure

```
session:   rounds with new inferences (pipelined)
           round to include pending txs (if signatures missing)
finalize:  round with MsgRevealSeed, no new inferences
settle:    MsgSettleEscrow to mainnet
```

Each step uses the same primitive: diffs in round-robin order.


## Interface Boundaries

Every subnet subpackage exposes a minimal interface in a dedicated `interface.go` file. The full subnet must be testable without mainnet, dapi, or containers.

### Mainnet Boundary

All mainnet interaction is behind one interface:

```go
// subnet/bridge/interface.go

type MainnetBridge interface {
    OnEscrowCreated(escrow EscrowInfo) error
    OnSettlementProposed(escrowID string, stateRoot []byte, nonce uint64) error
    OnSettlementFinalized(escrowID string) error
    GetEscrow(escrowID string) (*EscrowInfo, error)
    GetValidatorInfo(validatorAddress string) (*ValidatorInfo, error)
    VerifyWarmKey(warmAddress, validatorAddress string) (*WarmKeyInfo, error)
    SubmitDisputeState(escrowID string, stateRoot []byte, nonce uint64, sigs map[uint32][]byte) error
}
```

7 methods. Full definition with types in the Chain Data Requirements section below.

`OnSettlementProposed` fires when MsgSettleEscrow is submitted and the dispute window starts. Hosts compare proposed nonce against local state and may dispute. `OnSettlementFinalized` fires when the dispute window ends and settlement is final. Host cleans up local session state.

In production, `decentralized-api` provides an implementation that talks to the local chain node. In tests, a struct literal with preset return values drives the full subnet through any scenario.

This boundary is deliberately narrow and expensive by design (see Bridge Cost Model below). The subnet never polls mainnet, never subscribes to block events directly, never parses Cosmos SDK types. It derives what it can locally (slot assignment, nonce verification) and only asks the bridge what it cannot compute.

### Per-Package Interfaces

Each subpackage defines its own interface file. The package never imports concrete implementations from sibling packages directly. Wiring happens at the top level.

```
subnet/
  bridge/interface.go        # MainnetBridge
  state/interface.go         # StateMachine (apply diffs, verify nonces)
  storage/interface.go       # Storage (already defined above)
  signing/interface.go       # Signer, Verifier
  gossip/interface.go        # GossipClient (notify peers)
```

This means any component can be replaced with a test double. A test can run the full state machine with in-memory storage, a no-op gossip client, and a fake mainnet bridge. No network, no disk, no containers.

### In-Process Unit Tests

The target: a Go test file that creates a subnet session, sends inference requests, collects signatures, and settles -- all in-process, all deterministic. All nodes run in the same process, no network I/O. The test constructs the dependency graph manually:

```go
bridge := &FakeBridge{escrows: map[string]EscrowInfo{...}}
store  := storage.NewMemory()
signer := signing.NewSecp256k1(privateKey)
gossip := &NoOpGossip{}

node := subnet.New(bridge, store, signer, gossip)
// now drive the full protocol with real data
```

No docker-compose, no chain binary, no dapi binary. Every scenario from README.md (happy path, host down, user withholds data, recovery protocol) is testable this way. Simulation speed is limited only by CPU, not by block times or network latency.

### Multi-Node Integration Tests

Unit tests cover one node in-process. Integration tests cover a real subnet cluster: multiple nodes running as separate processes, communicating over real HTTP, with real gossip, real storage, real signing. The only mock is `MainnetBridge`.

Each node is a standalone binary (or a Go test spawning goroutines with real listeners). A test harness spins up N nodes, injects escrow info through the fake bridge, then drives user traffic against the cluster. Nodes gossip to each other over localhost. Storage is real SQLite (or PostgreSQL). Signatures are real secp256k1.

This is the level where stress testing happens. Scenarios:

- 1000 concurrent sessions across 30 nodes, measure throughput and latency
- Kill nodes mid-session, verify recovery protocol works end-to-end
- Inject malicious user behavior (withhold diffs, skip hosts, submit stale state)
- Race conditions: concurrent writes, signature arrival ordering, nonce conflicts

The fake bridge is trivial -- a shared in-memory map protected by a mutex. It returns preset escrow data and records settlement calls. No chain, no blocks, no Cosmos SDK, but the rest of the system is production code running under production conditions.

This is the key payoff of the narrow mainnet boundary: the entire subnet is real, only the 7-method bridge is fake. Stress tests hit real concurrency, real network, real disk I/O.


## Bridge Cost Model

Current dapi assumes RPC communication with the chain node is cheap. It queries freely: participant lists, authz grants, escrow state, epoch info. This works because dapi runs alongside its own node on the same machine.

The subnet library has the opposite assumption: bridge calls are expensive. The design minimizes them. Most calls happen once at session start. Subsequent calls happen only when something unexpected occurs (unknown warm key, failed verification, missing data).

This matters for deployment. Initially the subnet runs inside dapi and the bridge is a local function call to the existing chain client. Later, the subnet can be deployed as a standalone thin binary where the bridge is an RPC connector to a separate mainnet node. The narrow bridge makes both deployments possible without code changes.

Consequence: the subnet derives everything it can locally. Slot assignment is a deterministic function of (app_hash, escrow_id, validator_weights) -- the subnet computes it, never asks the bridge for it. Validator account addresses and public keys are loaded once at session start via `GetValidatorInfo` and cached. Warm key grants are cached on first contact and verified via bridge only on cache miss. The bridge provides only what cannot be derived: escrow existence, validator info, warm key authorization.


## Chain Data Requirements

The subnet needs a small set of data from mainnet. All of it flows through the `MainnetBridge` interface.

### What the Subnet Needs

1. Escrow info: amount, creator address, creation height, app_hash at creation.
2. Validator list and weights for the current epoch (to derive slot assignment locally).
3. Validator account addresses, public keys, and primary URLs (from `participant.inference_url` on chain). These are loaded at session start via `GetValidatorInfo` and serve as the cold key reference for signature verification -- no bridge call needed to verify a validator signing with its own key.
4. Warm key verification: given a (warm_address, validator_address) pair, confirm the authz grant exists. Called only on cache miss.

Slot assignment is derived locally from items 1+2 using the same `GetSlotsFromSorted` algorithm as PoC. The bridge never provides slot assignment directly.

### Signing and Verification

Mainnet uses Cosmos SDK's secp256k1 module for signature verification. That module is heavyweight and depends on the full SDK. The subnet cannot import it (no Cosmos SDK dependency).

The subnet uses `go-ethereum/crypto` for secp256k1 operations. This is the same library Ethereum has used since its initial version -- well-tested, standalone, no chain dependencies. The key operation is `ecrecover`: given a message hash and signature, recover the signer's public key and derive their address. No public key lookup needed.

Verification flow:

1. Message includes `validator_address` (the validator the signer claims to act for).
2. Receiver runs `ecrecover(message_hash, signature)` -> recovers signer address.
3. Receiver checks: is the recovered address the validator itself (cold key) or an authorized warm key?
4. For cold key: the recovered address matches `ValidatorInfo.Address` loaded at session start. No bridge call, no cache lookup.
5. For warm key: check the local warm key cache first. If found, done. If not found (first contact or new grant mid-session), call `bridge.VerifyWarmKey(recoveredAddress, validatorAddress)` to confirm the authz grant exists, then cache the result for the session. Bridge is only called on cache miss.

A host can sign with either its cold key (validator's own account key) or its warm key (operational key authorized via authz grant on mainnet). Both are secp256k1. Cold key signing requires no grant -- the validator is acting directly.

```go
type WarmKeyInfo struct {
    ValidatorAddress string
}
```

### Host Discovery: Multi-URL Identity

Each validator has a primary URL recorded on mainnet as `participant.inference_url`. This is the entrypoint. All initial discovery goes through it.

A validator may run multiple dapi instances behind this entrypoint, each capable of serving different subnets. The `/v1/identity` endpoint advertises which instances are available:

```go
type IdentityData struct {
    Address        string            `json:"address"`
    WarmKeyAddress string            `json:"warm_key_address"`
    Block          int64             `json:"block"`
    Timestamp      string            `json:"timestamp"`
    DelegateTAs    []DelegateGroup   `json:"delegate_tas,omitempty"`
}

type DelegateGroup struct {
    URL            string `json:"url"`
    WarmKeyAddress string `json:"warm_key_address"`
}
```

`DelegateTAs` is an indexed list of (URL, warm key) pairs. Selection for a given subnet is deterministic:

```
groupIndex = hash(escrow_id, app_hash) % len(DelegateTAs)
```

All subnet participants compute the same index for each host, so everyone agrees on which URL and warm key to use. If `DelegateTAs` is empty or has one entry, the primary URL is used (the common case at launch).

- Phase 1: single dapi, `DelegateTAs` has one entry or is omitted. No behavioral change.
- Phase N: validator runs 4 dapi instances. Each advertises itself as a delegate group. Subnets get distributed across instances deterministically.

### Discovery Flow

When a subnet session starts:

1. The node has escrow info (from `OnEscrowCreated` notification or `GetEscrow` query).
2. The node derives the slot assignment locally from (app_hash, escrow_id, validator weights).
3. For each validator in the assignment, the node fetches `/v1/identity` from the validator's primary URL (from `participant.inference_url` on chain, provided by `GetValidatorInfo`).
4. The response includes `DelegateTAs`. The node selects the entry at `hash(escrow_id, app_hash) % len(DelegateTAs)`.
5. That entry's URL becomes the communication endpoint and its warm key becomes the expected signer for that host in this subnet.

Step 3 happens once per session start, not per request. The result is cached for the session lifetime.

### MainnetBridge Interface

Minimal. The subnet derives everything it can locally and only asks the bridge what it cannot compute.

```go
type MainnetBridge interface {
    // Notifications: mainnet -> subnet
    OnEscrowCreated(escrow EscrowInfo) error
    OnSettlementProposed(escrowID string, stateRoot []byte, nonce uint64) error
    OnSettlementFinalized(escrowID string) error

    // Queries: subnet -> mainnet
    GetEscrow(escrowID string) (*EscrowInfo, error)
    GetValidatorInfo(validatorAddress string) (*ValidatorInfo, error)
    VerifyWarmKey(warmAddress, validatorAddress string) (*WarmKeyInfo, error)

    // Actions: subnet -> mainnet
    SubmitDisputeState(escrowID string, stateRoot []byte, nonce uint64, sigs map[uint32][]byte) error
}

type ValidatorInfo struct {
    Address   string
    PublicKey []byte
    URL       string    // participant.inference_url from chain
    Weight    uint64
}

type WarmKeyInfo struct {
    ValidatorAddress string
}
```

7 methods. `GetEscrow` and `GetValidatorInfo` are called at session start. `VerifyWarmKey` is called lazily on first contact with an unknown warm key, then cached. `OnEscrowCreated`, `OnSettlementProposed`, and `OnSettlementFinalized` are push notifications from mainnet events. `OnSettlementProposed` triggers dispute checks; `OnSettlementFinalized` triggers local state cleanup. `SubmitDisputeState` is called by a host that detects stale settlement during the dispute window. In tests, all seven are trivial struct methods returning preset data.


## Storage

### Data Model

Per-escrow state is a chain of diffs. Each diff is one nonce increment.

```
escrow_state:
  escrow_id       string (PK)
  group           []slot_assignment   # from mainnet at creation
  balance         uint64              # remaining escrow, decremented on each StartInference
  latest_nonce    uint64
  settled         bool

diff:
  escrow_id       string (FK)
  nonce           uint64 (PK with escrow_id)
  txs             []SubnetTx           # serialized proto
  signatures      map[slot_id][]byte   # accumulated over time
  state_hash      []byte               # cumulative hash at this nonce
  created_at      timestamp
```

Append-only. Signatures for a diff may arrive later (lag by 1+ rounds), so the signatures field gets updated in place.

### Storage Interface

```go
type Storage interface {
    CreateSession(escrowID string, group []SlotAssignment, balance uint64) error
    AppendDiff(escrowID string, diff Diff) error
    AddSignature(escrowID string, nonce uint64, slotID uint32, sig []byte) error
    GetState(escrowID string) (*EscrowState, error)
    GetDiffs(escrowID string, fromNonce, toNonce uint64) ([]Diff, error)
}
```

### Implementations

Phase 1: single SQLite file, WAL mode. All writes go through a dedicated goroutine fed by a buffered channel. Reads are concurrent (WAL allows multiple readers). Total cost: 3 file descriptors (db + wal + shm) regardless of session count.

Why not shard (one DB per escrow): 1000 concurrent sessions would open ~3000 file descriptors, competing with network sockets under the same ulimit. A single DB eliminates this entirely. Channel-hop latency is microseconds, and SQLite comfortably handles 1000 writes/sec with WAL. The write goroutine serializes mutations without any per-session lock management.

Phase 2: PostgreSQL via `jackc/pgx/v5` (already a dapi dependency). Single `subnet_diffs` table partitioned by escrow_id or time range. Enables multi-instance dapi pointing at the same DB. Migration path: the Storage interface is the same, just swap the implementation at startup based on config.


## Gossip

### What Needs Gossiping

Two things with different frequency:

1. Nonce propagation (every request). After processing a user request, the host pushes the current nonce to K random peers. A skipped host has zero visibility into the session because the user carries diffs only to hosts it contacts. A skipped host never receives them. Nonce gossip is the only detection mechanism.

2. Lazy tx inclusion (failure-path only). When a host-proposed tx (MsgFinishInference, MsgValidation) is not included by the user after K rounds, the host pushes it to peers so they can refuse to sign until it's included.

### Pattern

After processing a user request, the host gossips the current nonce to K=3 random group members.

Detection probability for a skipped host_i (N=30, K=3). Each request causes one host to gossip to 3 random peers. Probability host_i is NOT among them: ~90%. Cumulative:

- After 5 requests: ~59% still unaware (41% detection)
- After 10 requests: ~35% still unaware (65% detection)
- After 20 requests: ~12% still unaware (88% detection)
- After 30 requests (one full round): ~4% still unaware (96% detection)

As long as majority of hosts are honest and gossiping, propagation is reliable. A few malicious hosts refusing to gossip only slightly reduces the rate.

**Re-propagation.** If a host receives a gossiped nonce but never receives the actual user request within 120 seconds, it re-propagates to K random peers. This serves two purposes:
- Amplifies coverage if the first wave missed some hosts
- Signals to the group that a gap was detected, preparing for recovery (MsgRequestPrompt)

Total cost per inference request: K=3 outbound HTTP calls with ~100 byte body. No persistent connections, no new ports. Group members are known from mainnet slot assignment, each already has a public URL.

### API Surface

New routes on the existing dapi public server:

```
POST /subnet/v1/sessions/{escrow_id}/gossip/nonce
  Body: { nonce: uint64, sender_slot: uint32 }
  Purpose: "nonce N has been processed"

POST /subnet/v1/sessions/{escrow_id}/gossip/txs
  Body: { txs: []SubnetTx, sender_slot: uint32 }
  Purpose: "user hasn't included these after K rounds, please track them"
```

No new ports. No peer discovery (group is deterministic from mainnet). Connection pooling with short idle timeout handles the 3 calls per request without accumulation.

### Future Optimization

K=3 random peers over REST is the simplest correct approach. Gossip can be optimized independently of the rest of the system since the interface is just "notify peers about nonce N." Possible future directions: libp2p gossipsub, QUIC transport, adaptive K based on group size, or persistent WebSocket connections within a session. None of these affect the subnet state machine or storage layer.


## Settlement

### What Mainnet Needs to Verify

Mainnet receives MsgSettleEscrow and must verify that the claimed usage and host_stats are correct. The mandatory finalizing round (see README.md Settlement) ensures all inferences are resolved and host_stats are final before settlement.

```
MsgSettleEscrow:
  escrow_id          string
  state_root         []byte                       # Merkle root after finalizing round
  nonce              uint64                       # latest nonce
  signatures         map[uint32][]byte            # slot_id -> sig over (state_root, escrow_id, nonce)
  rest_hash          []byte                       # Merkle sibling: hash(balance_bytes || inferences_hash)
  host_stats         map[uint32]HostStats
```

Mainnet verification:
1. Compute host_stats_hash from the submitted host_stats
2. Verify Merkle proof: hash(host_stats_hash || rest_hash) == state_root
3. Verify 2/3+ slot-weighted signatures over (state_root || escrow_id || nonce)
4. Settle: pay each host from escrow according to host_stats[slot].cost, refund remaining balance (escrow_amount - sum of all host costs) to user, record host_stats

The Merkle proof is constant size: one sibling hash (rest_hash). Mainnet never sees individual inference records or balance.

No balance field in the payload. Mainnet knows the escrow amount and computes the refund from the sum of host_stats[*].cost.

### Finalizing Round

Before settling, the user sends one round of empty requests (same endpoint, same format, no new MsgStartInference) in round-robin to the full group. Each host attaches pending MsgFinishInference, MsgRevealSeed, and any remaining MsgValidation. After the full round, all inferences are resolved, all seeds are revealed, validation compliance is checked, and host_stats are final. Not a special request type -- same diff format, just no new inferences.

### Dispute Window

When mainnet receives MsgSettleEscrow, settlement enters a dispute window of X blocks (TBD). Bridge notifies each host via `OnSettlementProposed(escrowID, stateRoot, nonce)`.

Host compares proposed nonce against its local latest_nonce:
- If proposed nonce < local latest_nonce AND the host has 2/3+ signatures for a higher nonce: host calls `SubmitDisputeState` with the newer state. User submitted stale state and is penalized (forfeits remaining escrow to hosts).
- If proposed nonce >= local latest_nonce: no dispute. Settlement finalizes after X blocks.

The host is passive -- it reacts to the bridge notification, doesn't poll. One new notification method plus one action method on the bridge.

### Host-Initiated Settlement

If the user disappears, any group member can submit MsgSettleEscrow after a timeout (TBD: wall-clock from last nonce or escrow expiry height set at creation). All hosts have full state within one round (propagated via diffs). If a host is missing recent state, it requests from other hosts via the public API endpoint. Same 2/3+ signature requirement, same dispute window.


## ML Node Integration

The subnet reuses the existing dapi infrastructure for ML node interaction. Two interfaces defined in the subnet package, implemented by dapi as thin adapters over existing code. Zero cosmos-sdk in subnet, minimal changes in dapi.

### What dapi Already Has

Inference execution and validation re-execution share the same core: send an OpenAI-compatible request to a vLLM node, collect response, extract logits and token counts. This logic lives in:

- `completionapi/` -- request modification (`ModifyRequestBody`), response parsing (`CompletionResponse` interface), streaming processor (`ExecutorResponseProcessor`), logit extraction. Pure HTTP + JSON, zero chain dependencies.
- `internal/server/public/proxy.go` -- streaming response handler. Pure HTTP.
- `broker/` -- ML node locking, retry on transport/5xx errors, model-based node selection (`DoWithLockedNodeHTTPRetry`, `LockNode`).
- `internal/validation/inference_validation.go` -- `validateWithPayloads()` re-executes inference with enforced tokens, `compareLogits()` computes similarity score. Core logic is pure compute, only depends on node URL and payloads.

Chain-coupled parts that the subnet does NOT need: MsgStartInference/MsgFinishInference/MsgValidation chain transactions (subnet tracks its own state), transfer agent logic, escrow validation. Authz verification is still needed for warm key grants.

### Subnet Interfaces

Defined at the subnet module root (`subnet/engine.go`, `subnet/types.go`). These are the contract between subnet and dapi.

```go
// subnet/engine.go

// InferenceEngine executes inference on an ML node.
// Implemented by dapi using existing broker + completionapi.
type InferenceEngine interface {
    Execute(ctx context.Context, req ExecuteRequest) (*ExecuteResult, error)
}

// ValidationEngine re-executes inference and compares logits.
// Implemented by dapi using existing broker + completionapi.
type ValidationEngine interface {
    Validate(ctx context.Context, req ValidateRequest) (*ValidateResult, error)
}
```

```go
// subnet/types.go

type ExecuteRequest struct {
    Model       string
    RequestBody []byte              // original OpenAI-compatible JSON
    Seed        int32
    Writer      http.ResponseWriter // receives streaming response
}

type ExecuteResult struct {
    ResponsePayload  []byte
    PromptPayload    []byte  // canonicalized request
    PromptHash       string
    ResponseHash     string
    PromptTokens     uint64
    CompletionTokens uint64
}

type ValidateRequest struct {
    Model           string
    PromptPayload   []byte
    ResponsePayload []byte
}

type ValidateResult struct {
    Similarity   float64 // 1.0 = identical, <0.99 = invalid
    ResponseHash string
}
```

### dapi Adapters

Adapters live in dapi, not in the subnet package. Each wraps existing functions with no modifications to the originals.

```
decentralized-api/
  internal/
    subnet/
      engine_adapter.go       # implements InferenceEngine
      validation_adapter.go   # implements ValidationEngine
      router.go               # mounts /subnet/v1/ routes, wires adapters
```

InferenceEngine adapter (~50-80 lines):
1. `completionapi.ModifyRequestBody(requestBody, seed)` -- existing
2. `broker.DoWithLockedNodeHTTPRetry(broker, model, ...)` -- existing, broker used as-is
3. `http.Post(completionsUrl, body)` -- existing pattern
4. `completionapi.NewExecutorResponseProcessor` + `proxyResponse` -- existing, streams to writer
5. Extract hash + usage from `responseProcessor.GetResponse()` -- existing
6. Return `ExecuteResult`

ValidationEngine adapter (~40-60 lines):
1. `broker.LockNode(broker, model, ...)` -- existing
2. Parse payloads, extract enforced tokens via `completionapi` -- existing
3. POST to mlnode with enforced tokens -- existing pattern from `validateWithPayloads`
4. Compare logits via `compareLogits()` -- existing
5. Return `ValidateResult`

### Integration Point

The subnet mounts on the existing dapi server as a new echo router group:

```go
// decentralized-api/internal/subnet/router.go

func Mount(group *echo.Group, engine InferenceEngine, validator ValidationEngine, ...) {
    host := subnet.NewHost(engine, validator, storage, signer, ...)
    group.POST("/sessions/:escrow_id/chat/completions", host.HandleInference)
    group.POST("/sessions/:escrow_id/gossip/nonce", host.HandleNonceGossip)
    group.POST("/sessions/:escrow_id/gossip/txs", host.HandleTxGossip)
}
```

The dapi server startup wires the adapters and mounts the group at `/subnet/v1/`. The subnet handler receives user requests with diffs, applies them to subnet state, calls `InferenceEngine.Execute()`, creates MsgFinishInference for subnet state, signs the new state, returns signature + streaming response.


## Open Questions

1. Re-propagation timeout: 120s is a starting point. Should it scale with model latency or be fixed?
