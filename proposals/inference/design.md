# Inference Subnet: Technical Design

Working document. Captures design decisions and open questions for the subnet implementation described in [README.md](./README.md).

## No Cosmos SDK in Subnet

The subnet package has zero dependency on Cosmos SDK. Rationale: Cosmos SDK is slow, heavyweight, and the subnet is not a blockchain.

Mainnet keeps Cosmos SDK for `MsgCreateEscrow` and `MsgSettleEscrow`, both in `inference-chain/x/inference/`. The subnet package imports nothing from `inference-chain`.

Crypto primitives (signing, hashing) come from Go stdlib or standalone libs (e.g., `crypto/ed25519`, `crypto/sha256`). Proto definitions are self-contained within the subnet package.


## Transaction List

### Mainnet (2 txs)

Defined in `inference-chain/proto/` alongside existing 43 tx types.

| Tx | Proposer | Purpose |
|----|----------|---------|
| MsgCreateEscrow | user | Lock funds, triggers group sampling |
| MsgSettleEscrow | user or host | Finalize session, distribute escrow |

### Subnet (6 txs)

Defined in the subnet package's own proto files. No shared types with mainnet protos.

| Tx | Proposer | Purpose |
|----|----------|---------|
| MsgStartInference | user | Authorize inference, reserve cost from escrow balance |
| MsgFinishInference | host | Record completion, response hash, token counts |
| MsgValidation | host | Validation result. valid=true -> validated. valid=false -> opens challenge voting |
| MsgValidationVote | host | Vote during challenge window (after MsgValidation valid=false) |
| MsgTimeoutInference | host | Declare inference timed out. Carries timestamp. First per inference_id wins, duplicates ignored |
| MsgRequestPrompt | host | Recovery: request prompt data the user withheld |

8 total (2 mainnet + 6 subnet), down from 43 on mainnet today.

No separate `MsgInvalidateInference`. Invalidation is the result of a challenge voting round: `MsgValidation(valid=false)` opens the vote, `MsgValidationVote` collects votes, majority decides. This replaces the current mainnet pattern where `Validation` and `InvalidateInference` are separate RPCs.

MsgTimeoutInference dedup: content-addressed by inference_id. The state machine applies the first one and ignores subsequent ones for the same inference. Each applying host validates the timestamp against its own clock -- if the inference deadline hasn't passed by the host's clock, it rejects the diff. Clock skew tolerance of a few seconds is acceptable because T is tens of seconds minimum.


## Package Structure

Top-level Go module: `subnet/` at repo root. Imported by `decentralized-api` as a library.

### Both Roles in One Library

The library implements both the host flow and the user flow. The state machine is the same for both -- same diffs, same signature verification, same nonce tracking, same state hash computation. The difference is who proposes which transactions and who drives sequencing.

Host-specific: receive and validate incoming requests, sign state on receipt, propose MsgFinishInference/MsgValidation, gossip nonces, enforce inclusion rules.

User-specific: create MsgStartInference, sequence diffs, pick next host in round-robin, collect signatures, attach accumulated diffs to outgoing requests, submit settlement.

Both roles share: state machine, diff application, signature verification, nonce tracking, storage, types. This is the bulk of the code. Role-specific logic is a thin layer on top.

Why this matters: a single Go test can create 30 host nodes and 1 user node from the same library, drive a full session end-to-end, and verify everything in-process. No separate client SDK to maintain, no protocol drift between implementations. When we need a JS or Python client later, the Go library is the reference implementation and the wire protocol is the contract.

```
subnet/
  go.mod                    # standalone module, no cosmos-sdk
  proto/                    # subnet-specific proto definitions
  types/                    # generated proto types + domain types
  state/                    # state machine: apply diffs, verify nonces, track balances
  host/                     # host role: request handling, signing, gossip, inclusion enforcement
  user/                     # user role: sequencing, round-robin, signature collection, settlement
  signing/                  # signature creation and verification
  storage/                  # storage interface + implementations
  gossip/                   # gossip client and handlers
  bridge/                   # MainnetBridge interface
```

First release: `decentralized-api` imports `subnet/` and mounts a new echo router group on the existing public server port, e.g. `/subnet/v1/`. The user flow is available as a Go client library (`subnet/user`) for integration tests and future standalone client tooling.


## State Machine

### Session State

The state is a small struct, not a history. History lives in diffs (storage). The state is the current snapshot after applying all diffs up to latest_nonce.

```
SessionState:
  escrow_id            string
  balance              uint64                          # remaining escrow
  inferences           map[uint64]InferenceRecord      # keyed by inference_id
  host_stats           map[uint32]HostStats            # keyed by slot_id
  usage                UsageStats
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

UsageStats:
  total_cost           uint64
  total_input_tokens   uint64
  total_output_tokens  uint64
```

Signatures are NOT part of SessionState. They are stored alongside diffs in storage. Signatures attest to a state_hash at a given nonce but are not inside the state itself.

### Inference Lifecycle

```
started -> finished                                     (happy path, most common)
started -> finished -> challenged -> validated           (challenge dismissed by majority)
started -> finished -> challenged -> invalidated         (challenge confirmed by majority)
started -> timed_out                                     (host never finished)
```

Validation is probabilistic. Most inferences are never validated -- `started -> finished` is the normal terminal path.

Transitions and state updates:

- MsgStartInference: creates record with status=started, reserves estimated cost from balance, updates usage (cost only -- tokens unknown yet).
- MsgFinishInference: status=finished, records response_hash and actual token counts, finalizes cost (adjusts balance and usage if actual differs from estimate).
- MsgValidation(valid=true): status=validated. No change to usage or host_stats.
- MsgValidation(valid=false): status=challenged, opens voting window.
- MsgValidationVote: increments votes_valid or votes_invalid. When votes_invalid > group_size/2: status=invalidated, host_stats[executor].invalid += 1, usage.total_cost -= cost, balance += cost (user refund -- shouldn't pay for bad output). When votes_valid > group_size/2 or voting window expires: status=validated.
- MsgTimeoutInference: status=timed_out, host_stats[executor].missed += 1. Only valid if current time > deadline. Cost stays charged (host reserved the slot even if it failed). Usage already reflects the reserved cost from MsgStartInference.

### State Pruning

Once an inference reaches a terminal state (finished, validated, invalidated, timed_out), its cost and token counts are folded into usage and host_stats. The record can be removed from the inferences map. Only in-flight and recently-finished-but-within-challenge-window inferences stay. This keeps the map bounded by concurrent inferences + challenge window size, not total session length.

Pruning rule: prune when status is terminal AND challenge window has expired (so late MsgValidation for that inference is no longer valid). A pruned inference_id is tracked in a tombstone set to reject late-arriving txs that reference it.

### State Hash and Merkle Structure

The state is structured as a Merkle tree:

```
           state_root
          /          \
 settlement_hash    inferences_root
```

settlement_hash = hash(serialize(usage) || serialize(host_stats)). inferences_root = hash of the inferences map. state_root = hash(settlement_hash || inferences_root). Serialization is deterministic (protobuf with sorted map keys, fixed field order).

This structure enables settlement without a flush round: mainnet only needs usage + host_stats in plaintext, plus inferences_root as the Merkle sibling. Mainnet recomputes settlement_hash, verifies it against state_root, and checks signatures. The inferences subtree is opaque to mainnet. See Settlement section.

Every host applying the same diffs to the same nonce produces the same state_root. This is the value that gets signed.

### What Gets Signed

A host signs: `sign(state_root || escrow_id || nonce)`. The escrow_id prevents cross-session replay. The nonce prevents cross-nonce replay. The state_root binds the signature to a specific state. Two hosts signing the same (escrow_id, nonce) must have seen identical transactions.

Signing happens before the host begins streaming the inference response. The host validates the incoming diffs, applies them, computes the new state_root, and signs. This is the "acknowledge receipt" signature. After execution completes, the host produces MsgFinishInference (a separate tx, included in a later diff by the user).

### Diff Application

When a host receives a request with diffs:

1. For each diff from local_latest_nonce+1 to received_latest_nonce:
   a. Validate: nonce is sequential, txs are well-formed, proposer is authorized
   b. Apply each tx to SessionState (update balance, inferences, host_stats, usage)
   c. Check active inferences for timeout (compare deadline against host's clock)
   d. Prune terminal inferences past their challenge window
   e. Compute state_root (Merkle tree)
   f. Store diff + state_root
2. Verify included signatures against stored state_roots at their respective nonces
3. Append new diff (current nonce) with the user's new txs
4. Sign new state_root, return signature

If any diff fails validation, reject the entire request. The host never applies partial diffs.


## Interface Boundaries

Design principle: every subnet subpackage exposes a minimal interface in a dedicated `interface.go` file. The full subnet must be testable without mainnet, without dapi, without containers. Feed data into interfaces, get results out.

### Mainnet Boundary

The subnet knows nothing about Cosmos SDK, gRPC, or chain internals. All mainnet interaction is behind a single interface:

```go
// subnet/bridge/interface.go

type MainnetBridge interface {
    OnEscrowCreated(escrow EscrowInfo) error
    OnEscrowSettled(escrowID string) error
    OnSettlementProposed(escrowID string, stateRoot []byte, nonce uint64) error
    GetEscrow(escrowID string) (*EscrowInfo, error)
    GetValidatorInfo(validatorAddress string) (*ValidatorInfo, error)
    VerifyWarmKey(warmAddress, validatorAddress string) (*WarmKeyInfo, error)
    SubmitDisputeState(escrowID string, stateRoot []byte, nonce uint64, sigs map[uint32][]byte) error
}
```

7 methods. Full definition with types in the Chain Data Requirements section below.

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

### Testing Without Infrastructure

The target: a Go test file that creates a subnet session, sends inference requests, collects signatures, and settles -- all in-process, all deterministic. The test constructs the dependency graph manually:

```go
bridge := &FakeBridge{escrows: map[string]EscrowInfo{...}}
store  := storage.NewMemory()
signer := signing.NewEd25519(privateKey)
gossip := &NoOpGossip{}

node := subnet.New(bridge, store, signer, gossip)
// now drive the full protocol with real data
```

No docker-compose, no chain binary, no dapi binary. Every scenario from README.md (happy path, host down, user withholds data, recovery protocol) is testable this way. Simulation speed is limited only by CPU, not by block times or network latency.

### Multi-Node Integration Tests

Unit tests cover one node in-process. Integration tests cover a real subnet cluster: multiple nodes running as separate processes, communicating over real HTTP, with real gossip, real storage, real signing. The only mock is `MainnetBridge`.

Each node is a standalone binary (or a Go test spawning goroutines with real listeners). A test harness spins up N nodes, injects escrow info through the fake bridge, then drives user traffic against the cluster. Nodes gossip to each other over localhost. Storage is real SQLite (or PostgreSQL). Signatures are real ed25519.

This is the level where stress testing happens. Scenarios:

- 1000 concurrent sessions across 30 nodes, measure throughput and latency
- Kill nodes mid-session, verify recovery protocol works end-to-end
- Inject malicious user behavior (withhold diffs, skip hosts, submit stale state)
- Race conditions: concurrent writes, signature arrival ordering, nonce conflicts

The fake bridge is trivial -- a shared in-memory map protected by a mutex. It returns preset escrow data and records settlement calls. No chain, no blocks, no Cosmos SDK, but the rest of the system is production code running under production conditions.

This is the key payoff of the narrow mainnet boundary: the entire subnet is real, only the 5-method bridge is fake. Stress tests hit real concurrency, real network, real disk I/O.


## Bridge Cost Model

Current dapi assumes RPC communication with the chain node is cheap. It queries freely: participant lists, authz grants, escrow state, epoch info. This works because dapi runs alongside its own node on the same machine.

The subnet library has the opposite assumption: bridge calls are expensive. The design minimizes them. Most calls happen once at session start. Subsequent calls happen only when something unexpected occurs (unknown warm key, failed verification, missing data).

This matters for deployment. Initially the subnet runs inside dapi and the bridge is a local function call to the existing chain client. Later, the subnet can be deployed as a standalone thin binary where the bridge is an RPC connector to a separate mainnet node. The narrow bridge makes both deployments possible without code changes.

Consequence: the subnet derives everything it can locally. Slot assignment is a deterministic function of (app_hash, escrow_id, validator_weights) -- the subnet computes it, never asks the bridge for it. Warm key verification is on-demand per message, not preloaded per session. The bridge provides only what cannot be derived: escrow existence, validator public keys, warm key authorization checks.


## Chain Data Requirements

The subnet needs a small set of data from mainnet. All of it flows through the `MainnetBridge` interface.

### What the Subnet Needs

1. Escrow info: amount, creator address, creation height, app_hash at creation.
2. Validator list and weights for the current epoch (to derive slot assignment locally).
3. Validator public keys and primary URLs (from `participant.inference_url` on chain).
4. Warm key verification: given a (warm_address, validator_address) pair, confirm the grant exists and return the public key.

Slot assignment is derived locally from items 1+2 using the same `GetSlotsFromSorted` algorithm as PoC. The bridge never provides slot assignment directly.

### Warm Key Rule

Problem: a host signs a subnet message with its warm key. The receiver needs to verify. Without knowing which warm key was used, the receiver must iterate all warm keys for all validators in the group. For a 30-host group with 2 warm keys each, that is 60 trial verifications per message.

Rule: every signed subnet message includes the signer's warm key address as a plaintext field alongside the validator address it claims to act for. The receiver verifies exactly one pair:

1. Message says: "signed by warm_address X on behalf of validator Y."
2. Receiver calls `bridge.VerifyWarmKey(warmAddress, validatorAddress)` -> public key.
3. One signature verification against that public key. Done.

No preloaded map. No iteration. The bridge call is cached locally after the first successful verification for a given (warm_address, validator_address) pair. If a warm key is not in cache (new grant mid-session, or first contact), the bridge is called once. If verification fails, the message is rejected.

```go
type WarmKeyInfo struct {
    ValidatorAddress string
    PublicKey        []byte
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
    OnEscrowSettled(escrowID string) error
    OnSettlementProposed(escrowID string, stateRoot []byte, nonce uint64) error

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
    PublicKey        []byte
}
```

7 methods. `GetEscrow` and `GetValidatorInfo` are called at session start. `VerifyWarmKey` is called lazily on first contact with an unknown warm key, then cached. `OnEscrowCreated`, `OnEscrowSettled`, and `OnSettlementProposed` are push notifications from mainnet events. `SubmitDisputeState` is called by a host that detects stale settlement during the dispute window. In tests, all seven are trivial struct methods returning preset data.


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

### Pattern: Push to K Random Peers

Each host that processes a user request gossips the nonce to K=3 random group members. No fan-out to all N. No re-gossip chain: only hosts that directly receive user traffic push to K each.

Detection probability for a skipped host_i (N=30, K=3). After each subsequent request, one host gossips to 3 random peers. Probability host_i is NOT among them: ~90%. Cumulative:

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

Mainnet receives MsgSettleEscrow and must verify that the claimed usage and host_stats are correct. The challenge: mainnet doesn't have the full subnet state, only what the settlement tx includes.

The Merkle state structure solves this. The settlement-relevant data is usage + host_stats, hashed together into a single settlement_hash. The settlement payload:

```
MsgSettleEscrow:
  escrow_id          string
  state_root         []byte                       # Merkle root of full SessionState
  nonce              uint64                       # latest nonce
  signatures         map[uint32][]byte            # slot_id -> sig over (state_root, escrow_id, nonce)
  settlement_hash    []byte                       # hash(usage || host_stats)
  inferences_root    []byte                       # sibling hash (Merkle proof)
  usage              UsageStats                   # plaintext
  host_stats         map[uint32]HostStats         # plaintext
```

Mainnet verification:
1. Recompute settlement_hash from plaintext: hash(serialize(usage) || serialize(host_stats))
2. Verify Merkle proof: hash(settlement_hash || inferences_root) == state_root
3. Verify 2/3+ slot-weighted signatures over (state_root || escrow_id || nonce)
4. Settle: pay hosts from escrow proportionally to usage.total_cost, refund (escrow_amount - usage.total_cost) to user, record host_stats

No balance field in the payload. Mainnet knows the escrow amount and computes the refund from usage.total_cost.

The Merkle proof is constant size: one sibling hash (inferences_root). Mainnet never sees individual inference records. The user can settle at any point regardless of active inferences.

> Note: this works but a more elegant approach may exist. The Merkle tree adds complexity to the state hash computation. An alternative worth exploring: a separate settlement_hash signed by hosts at flush time, covering only settlement-relevant fields. Or a commitment scheme where hosts periodically sign usage checkpoints. The current Merkle approach is correct and avoids requiring a flush round, which is the priority.

### Flush Round

Optional optimization. Before settling, the user sends one round of empty requests (same endpoint, same format, no new MsgStartInference) to collect remaining signatures and let hosts attach pending MsgFinishInference. Not a special request type.

With the Merkle settlement approach, the flush round is not required for correctness. But it produces a cleaner final state (empty inferences map, all work accounted for). Without flush, the user may forfeit cost of unfinished inferences (their cost is reserved in usage but no MsgFinishInference adjusts the final amount).

### Dispute Window

When mainnet receives MsgSettleEscrow, settlement enters a dispute window of X blocks (TBD). Bridge notifies each host via `OnSettlementProposed(escrowID, stateRoot, nonce)`.

Host compares proposed nonce against its local latest_nonce:
- If proposed nonce < local latest_nonce AND the host has 2/3+ signatures for a higher nonce: host calls `SubmitDisputeState` with the newer state. User submitted stale state and is penalized (forfeits remaining escrow to hosts).
- If proposed nonce >= local latest_nonce: no dispute. Settlement finalizes after X blocks.

The host is passive -- it reacts to the bridge notification, doesn't poll. One new notification method plus one action method on the bridge.

### Host-Initiated Settlement

If the user disappears, any group member can submit MsgSettleEscrow after a timeout (TBD: wall-clock from last nonce or escrow expiry height set at creation). All hosts have full state within one round (propagated via diffs). If a host is missing recent state, it requests from other hosts via the public API endpoint. Same 2/3+ signature requirement, same dispute window.


## Open Questions

1. Signature scheme: ed25519 (simple, per-host sigs) or BLS (aggregatable, smaller settlement tx)?
4. Re-propagation timeout: 120s is a starting point. Should it scale with model latency or be fixed?
5. Session lifecycle: how does a host learn about new escrows assigned to its group? Mainnet event subscription or lazy discovery on first user request?
