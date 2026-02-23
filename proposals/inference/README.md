# Inference Scaling 

## Problem 

Per inference, the following transactions are recorded on-chain:
- MsgStartInference
- MsgFinishInference
- MsgValidation (0 to N_hosts per inference; 1 on average in the normal case)

3 txs per inference. Max capacity per block is ~5000
=> 5000 / 3 = 1666 inferences per block
=> 1666 / 6 = 277 inferences per sec

Consider 4xH100 with Qwen3-235B deployed.
For 5000/1000 input/output tokens, such a setup can process 3.5-4 RPS (TODO: confirm)
=> 277 / 3.5 * 4 = 316 H100 GPUs to saturate the chain

Requests could be batched into a single transaction, but the computation and state growth per request makes this not scalable to hundreds of thousands of inferences.

The bottleneck is better with longer requests (more compute per tx) and worse with smaller models (more RPS per GPU, more txs per GPU).

> Note: in practice, the main limit is not the transaction count but the computation cost per block.
> It becomes a problem above a few hundred such transactions per block.
> Based on profiling it can be optimized 2-10x (or even 100x), but this limitation will hit before the tx count limit.
> The current proposal still tries to address the whole problem.


## Proposal

This proposal describes an approach that moves all per-inference communication off-chain.
The chain processes only two transactions: one to put coins in escrow and assign a subgroup of hosts, one to settle at the end.
All inference communication and validations happen inside the subgroup directly, over a long session (e.g. one epoch).
To close the session, the user submits the final usage state signed by a majority of hosts (threshold: 1/2 or 2/3, TBD).
Both sides have a clear incentive to settle: the user recovers the unused escrow balance, and the subgroup gets paid from it.

Effectively, as each subgroup would have to achieve consensus for the final state, the architecture will consist of:
- main blockchain
- many sub-chains / shards with extremely lightweight architecture 

Sub-chains will be able to process only the inference related transactions and their decision might affect only the escrows, assigned to such sub-chains

> Note: "sub-chain" does not have to mean a real blockchain. Because the group carries no state outside of its assigned user, groups can be dynamic: formed per session, with large overlaps between them. The only thing they share is the mainnet escrow as anchor.

### Architecture

```
+-----------+     +-------------------+     +----------------------------+
|   User    |     |      Mainnet      |     |  Subnet (one per session)  |
+-----------+     +-------------------+     +----------------------------+
      |                    |                             |
      | 1. MsgCreateEscrow |                             |
      |    (100GNK)        |                             |
      | -----------------> |                             |
      | <- escrow_id,      |                             |
      |    group=[h1..hN]  |                             |
      |                    |                             |
      | 2. POST /chat (req1) --------------------------> |
      | 3. POST /chat (req2) --------------------------> |
      | 4. POST /chat (reqN) --------------------------> |
      |    ...             |                             |
      |                    |                             |
      | 5. MsgSettleEscrow |                             |
      |   (finalState,     |                             |
      |    signatures, ..) |                             |
      | -----------------> |                             |
      | <- user refund +   |                             |
      |    hosts paid      |                             |
+-----------+     +-------------------+     +----------------------------+
```

User sends exactly 2 transactions to mainnet: `MsgCreateEscrow` to open the session, `MsgSettleEscrow` to close it.
All inference requests happen directly with the assigned subnet group; mainnet never sees individual requests.

### User Flow

- [mainchain]: user creates `MsgCreateEscrow(100GNK)` 
- [subchain]: user interact with hosts in subgroup in pre-defined order
- [mainnet]: at the end of session, user creates `MsgSettleEscrow(finalState, signatures, missed, invalid)`

Q1: Who decides host punishments, the subchain or mainnet?

If the subchain decides: it needs to aggregate stats across users, which requires shared persistent state per group, which requires fixed groups rather than dynamic per-user ones.

Current approach: mainnet decides. The subchain only records raw per-session stats (missed/invalid counts per host) inside `MsgSettleEscrow`. Mainnet aggregates across sessions and applies punishment. Can be revisited.

Q2: Do hosts maintain per-group state or per-user state?

If per-group: same consequence as Q1 option A, fixed groups required.

Current approach: per-user, following from Q1. Each host tracks only what happened inside each user session. No shared state between users in the same group. This is what makes dynamic per-session groups possible. Can be revisited.

The further proposal follows this architecture: "chain per user".


### Main Network Protocol

```
MsgCreateEscrow(
  creatorAddr
  amount,
)
```
1. move money to escrow via `MsgCreateEscrow`
2. return id to sample N(64?) slots-hosts using weighted random sampling (see [proposals/poc/optimize.md](../poc/optimize.md) for the slot idea)
3. interact in sub-chain during session 
4. settle on-chain via `MsgSettleEscrow`

```
MsgSettleEscrow(
  creatorAddr, # can be both user or someone from group
  finalState, # hash
  signatures, # signed by majority form group
  missed, # [groupMember -> uint32]
  invalid # [groupMember -> uint32]
)
```

5. On the escrow settlement, mainnet verifies signatures from subnet. Must be signed by majority / supermajority.
Once signatures are verified it settles escrow for the user, updates stats for hosts (missed, invalid).


### Subnet Protocol

The subnet is a lightweight shard with voting weight provided by mainnet. It settles back to mainnet when the session ends.

Design goals: lightweight, parallelizable, enforce that the user uses all hosts from the group.

What does the user want?
Send OpenAPI-compatible REST requests (`/chat/completions`, `/embeddings`, etc.) and know as little as possible about the blockchain.

What does the chain want?
Same properties we tried to achieve on mainnet:
- Know when each request starts and finishes. Other hosts measure executor performance against expected throughput and punish underperformance (missed rate).
- Know the hash of prompt (signed by user) and hash of response payload (signed by executor). Prompt signature authorizes payment. Payload signature enables probabilistic inference validation (invalid rate).
- Enforce distribution of requests across executors proportionally to their weight.

The chain needs these properties but does not want to process this data on mainnet.

----

**Per-user state.** State is saved per user independently. Each user's history is a chain of diffs. Each diff is essentially a block. Since there is no cross-user state, a node operator can shard its database and resources per user. Each node can participate in any number of subnets simultaneously. Subnet processing scales linearly with user count. Only escrow creation and settlement on mainnet do not.

**User-driven propagation.** The user is responsible for sequencing and propagating transactions. User attaches accumulated diffs to each inference request. This piggybacks propagation on normal API usage.

**Round-robin host ordering.** The user must iterate hosts in the group in a predefined order. This naturally distributes requests across hosts (not real work amount, but request count). Each diff carries a nonce, so the user cannot skip hosts.

**Signing flow.** When a `/chat/completions` request is sent to host1, the user creates MsgStartInference(1). If host1 is honest, it must immediately return `(state, signature)` without waiting for execution. After execution, host1 signs MsgFinishInference(1) and the user propagates it to the network in the next round (or later, depends on performance). Locks should only be needed to generate new nonces and compose new messages, not to record incoming data. The user does not block on receiving a host's signature before sending the next request. Signatures arrive asynchronously and get included in later diffs. This keeps request submission fast at the cost of signatures lagging behind by one or more rounds.

**Escrow accounting.** On each MsgStartInference, the subnet tracks spending against the user's escrow balance. Same idea as mainnet: verify user has enough funds before accepting the request. Minimum escrow balance must be at least `subnet_size * max_inference_cost` at all times, ensuring enough to cover the worst case where every host in the group is processing a concurrent request.

**Host unavailability.** If a host is not available, the user continues to the next host in order. Since each request carries ALL accumulated diffs for the current round, it includes the unsigned diff for the unavailable host. The receiving host follows the protocol to decide together with the group whether the unavailable host should be punished.

**Host-proposed transactions.** Hosts produce transactions (MsgFinishInference, invalidation triggers, etc.) that must be included in the state. The user is the sequencer, but cannot be trusted to include them. Propagation channels:
- Response body: host returns its proposed transactions to the user alongside the inference result.
- Lazy gossip: host pushes proposed transactions to other hosts only if the user hasn't included them after K rounds. Zero overhead in the happy path.
- Public endpoint: each host exposes its unsettled transactions per session. Fallback if lazy gossip fails.

**Inclusion enforcement.** Two different rules depending on who proposed the transaction:
- User-proposed (MsgStartInference): must appear in the very next round's diffs. The user has them at creation time, no reason for delay.
- Host-proposed (MsgFinishInference, etc.): K rounds grace period (TBD). Accounts for async lag. After K rounds without inclusion, hosts trigger lazy gossip and refuse to sign.

Each host response includes its unsettled mempool so the user always knows what's pending.

#### Scenarios

#### Everyone is working correctly

Group = [h1, h2, h3, h4, h5], user sends 3 requests in round-robin order.

```
User -> h1: POST /chat/completions (req1)
  diffs: [MsgStartInference(1)]
  h1: starts executing, signs state(nonce=1), returns (sig_h1, mempool=[])
  h1: after execution, creates MsgFinishInference(1), gossips to h2..h5

User -> h2: POST /chat/completions (req2)
  diffs: [MsgStartInference(1), MsgStartInference(2)]    // no sig_h1 yet
  h2: signs state(nonce=2), returns (sig_h2, mempool=[])

User -> h3: POST /chat/completions (req3)
  diffs: [MsgStartInference(1) + sig_h1,
          MsgStartInference(2) + sig_h2,
          MsgFinishInference(1),
          MsgStartInference(3)]
  h3: checks local mempool -- MsgFinishInference(1) present (via gossip), included, ok
  h3: signs state(nonce=3), returns (sig_h3, mempool=[])
```

Transaction statuses after 3 requests:
- MsgStartInference(1): settled (3 sigs: h1, h2, h3)
- MsgStartInference(2): proposed (2 sigs: h2, h3)
- MsgFinishInference(1): proposed (1 sig: h3)
- MsgStartInference(3): proposed (1 sig: h3)

The user is the sequencer: it decides at which nonce each transaction is placed. All hosts seeing the same nonce see the same content. Signatures lag behind by one or more rounds.

#### One (or minority) of hosts are not responding

TODO

#### Host doesn't return FinishInference

TODO

#### User creates StartInference but doesn't provide data to host_i

TODO

#### User sends request to host_i but doesn't record StartInference

TODO

### Example requests
```
/chat/completions -d '{
  "model":"Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
  "stream":true, "logprobs":true, "top_logprobs": 5,
  "messages":[
    {"role":"system","content":"You are a helpful assistant."},
    {"role":"user","content":"Write a haiku about Seattle."}
  ],
  "diffs": [
    {
      "txs": [MsgStartInference(1)], # sent with first /chat/completions
      "signatures": [sign1, sign2, sign], # 
    },
    {
      "txs": [MsgStartInference(2)],  # sent with second /chat/completions
    },
    {
      "txs": [MsgStartInference(3), MsgFinishInference(2), MsgFinishInference(1)], # sent with third /chat/completions
    },
    
    
    
    ...
  ],
  "last_state_hash": "<SHA256>"
}'
```

Q1: how exactly to propagate signatures? each diff essentially a new block and has it's own signatures
Q2: currently consider that ever


### Weights in subnet

Subnet group formation reuses the slot sampling mechanism from PoC validation (see [proposals/poc/optimize.md](../poc/optimize.md)).

Slot assignment is a deterministic function of (app_hash after escrow creation, escrow_id, validator_weights) using the same `GetSlotsFromSorted` algorithm as in PoC. The chain does not need to compute it at escrow creation. Anyone can derive the group independently. The chain only verifies the group was correct at settlement time (MsgSettleEscrow).

Each slot maps to a host. If a host is sampled into 3 slots, it has weight 3 in the subnet. Each slot carries weight 1. This preserves the mainnet weight distribution inside the subnet without requiring any additional weight tracking.

The slot sequence also defines the round-robin order for user requests.

Requirements for slot count are less strict than in PoC. In PoC, slots protect against adversarial validation (fake participant attacks). In the subnet, the group only needs enough redundancy for availability and settlement signatures. The exact slot count (64 vs 128) is TBD.

TODO: define settlement signature threshold relative to slot count
