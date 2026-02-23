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

### Subnet (4 txs)

Defined in the subnet package's own proto files. No shared types with mainnet protos.

| Tx | Proposer | Purpose |
|----|----------|---------|
| MsgStartInference | user | Authorize inference, deduct from escrow balance |
| MsgFinishInference | host | Record completion, response hash, token counts |
| MsgValidation | host | Submit validation result for a past inference |
| MsgRequestPrompt | host | Recovery: request prompt data the user withheld |

This is the full list. 6 total, down from 43 on mainnet today.

Open question: do we need `MsgInvalidateInference` as a separate tx, or is it a subtype/field inside `MsgValidation`? Current mainnet has both `Validation` and `InvalidateInference` as separate RPCs. Subnet could fold invalidation into the validation result (e.g., `valid: false` field).


## Package Structure

Top-level Go module: `subnet/` at repo root. Imported by `decentralized-api` as a library.

```
subnet/
  go.mod                    # standalone module, no cosmos-sdk
  proto/                    # subnet-specific proto definitions
  types/                    # generated proto types + domain types
  state/                    # state machine: apply diffs, verify nonces, track balances
  signing/                  # signature creation and verification
  storage/                  # storage interface + implementations
  gossip/                   # gossip client and handlers
```

First release: `decentralized-api` imports `subnet/` and mounts a new echo router group on the existing public server port, e.g. `/subnet/v1/`.


## Interface Boundaries

Design principle: every subnet subpackage exposes a minimal interface in a dedicated `interface.go` file. The full subnet must be testable without mainnet, without dapi, without containers. Feed data into interfaces, get results out.

### Mainnet Boundary

The subnet knows nothing about Cosmos SDK, gRPC, or chain internals. All mainnet interaction is behind a single interface:

```go
// subnet/mainnet/interface.go

type MainnetBridge interface {
    // Notifications: mainnet -> subnet
    OnEscrowCreated(escrow EscrowInfo) error
    OnEscrowSettled(escrowID string) error

    // Queries: subnet -> mainnet
    GetEscrow(escrowID string) (*EscrowInfo, error)
    GetSlotAssignment(escrowID string) ([]SlotAssignment, error)
}
```

In production, `decentralized-api` provides a real implementation that subscribes to chain events and queries the node. In tests, a struct literal with preset return values is enough to drive the full subnet through any scenario.

This boundary is deliberately narrow. The subnet never polls mainnet, never subscribes to block events directly, never parses Cosmos SDK types. It receives notifications and asks questions through this interface. Everything else is internal.

### Per-Package Interfaces

Each subpackage defines its own interface file. The package never imports concrete implementations from sibling packages directly. Wiring happens at the top level.

```
subnet/
  mainnet/interface.go       # MainnetBridge
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

This is the key payoff of the narrow mainnet boundary: the entire subnet is real, only the 4-method bridge is fake. Stress tests hit real concurrency, real network, real disk I/O.


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


## Open Questions

1. Signature scheme: ed25519 (simple, per-host sigs) or BLS (aggregatable, smaller settlement tx)?
4. Re-propagation timeout: 120s is a starting point. Should it scale with model latency or be fixed?
5. Session lifecycle: how does a host learn about new escrows assigned to its group? Mainnet event subscription or lazy discovery on first user request?
