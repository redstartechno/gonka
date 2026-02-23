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

## Architecture

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

## User Flow

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


## Main Network Protocol

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


## Subnet Protocol

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

**Host unavailability.** If a host is not available, the user continues to the next host in order. Since each request carries ALL accumulated diffs for the current round, it includes the unsigned diff for the unavailable host. Detection and recovery are handled via nonce propagation (see scenarios below).

**Nonce propagation.** After processing each user request, the receiving host gossips the current nonce to the group. Small constant-overhead message. Each host tracks the highest nonce seen. If host_i sees that nonce has advanced past its assigned position but was never contacted, it detects a gap and can act proactively. This is the only reliable detection mechanism: other hosts cannot distinguish "still computing" from "never received data" by looking at diffs (execution time varies), and signature lag is normal (signatures always trail by at least one round).

**Host-proposed transactions.** Hosts produce transactions (MsgFinishInference, invalidation triggers, etc.) that must be included in the state. The user is the sequencer, but cannot be trusted to include them. Propagation channels:
- Response body: host returns its proposed transactions to the user alongside the inference result.
- Lazy gossip: host pushes proposed transactions to other hosts only if the user hasn't included them after K rounds. Zero overhead in the happy path.
- Public endpoint: each host exposes its unsettled transactions per session. Fallback if lazy gossip fails.

**Inclusion enforcement.** Two different rules depending on who proposed the transaction:
- User-proposed (MsgStartInference): must appear in the very next round's diffs. The user has them at creation time, no reason for delay.
- Host-proposed (MsgFinishInference, etc.): K rounds grace period (TBD). Accounts for async lag. After K rounds without inclusion, hosts trigger lazy gossip and refuse to sign.

Each host response includes its unsettled mempool so the user always knows what's pending.

### Scenarios

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
  h3: checks local mempool:MsgFinishInference(1) present (via gossip), included, ok
  h3: signs state(nonce=3), returns (sig_h3, mempool=[])
```

Transaction statuses after 3 requests:
- MsgStartInference(1): settled (3 sigs: h1, h2, h3)
- MsgStartInference(2): proposed (2 sigs: h2, h3)
- MsgFinishInference(1): proposed (1 sig: h3)
- MsgStartInference(3): proposed (1 sig: h3)

The user is the sequencer: it decides at which nonce each transaction is placed. All hosts seeing the same nonce see the same content. Signatures lag behind by one or more rounds.

#### Host doesn't respond or doesn't finish inference

MsgStartInference(N) exists in the state but MsgFinishInference(N) never arrives. Possible causes:
- Host genuinely down, didn't receive the request
- Connection broke between user and host mid-request
- Host received data but refuses to compute
- User recorded MsgStartInference but withheld prompt data from the host

Attribution is hard. The user could attack a host by recording MsgStartInference but withholding prompt data. The host could attack by pretending not to have received it. Both look identical from the outside. Without a recovery mechanism, whoever is honest gets punished.

**If host_i signed the state at nonce N:** host_i acknowledged receipt. The signature propagates through later diffs, so all hosts can verify host_i had the data. If MsgFinishInference(N) doesn't arrive by timeout, missed += 1 for host_i. No ambiguity.

**If host_i never signed:** ambiguous. Recovery protocol applies.

**Recovery protocol:**
1. host_i detects via nonce propagation that a nonce assigned to it has passed without receiving data.
2. host_i gossips MsgRequestPrompt(N) to the group.
3. Each host that sees MsgRequestPrompt(N) independently includes it in its next response to the user: "provide prompt for nonce N."
4. A small relay group is sampled from the subnet using the mainnet block hash at 1 block after MsgRequestPrompt(N). This way host_i has already committed to the claim before learning who the relay group will be, and the user cannot preselect colluding intermediaries.
5. User provides prompt data to the relay group. Each member signs a receipt and relays to host_i independently.
6. host_i computes, produces MsgFinishInference(N). User can reconnect to host_i directly for the response, or receive it through a relay member.
7. If host_i still hasn't received the data, host_i can re-request with another MsgRequestPrompt.

If user doesn't provide prompt within R_prompt rounds (TBD), hosts refuse to sign further state updates. host_i not penalized.

If host_i receives prompt via relay but still doesn't finish by timeout, missed += 1. Multiple hosts can attest the prompt was delivered.

**Timeout.** Timestamp in MsgStartInference + T seconds. On mainnet, timeout was block-height-based (expirationHeight). In the subnet there are no blocks, so wall-clock time anchored to the StartInference timestamp is the replacement. T must account for the full recovery protocol (nonce propagation + MsgRequestPrompt + prompt relay + execution).

**Incentives.** The recovery protocol removes both attack vectors:
- User cannot selectively starve a host of data. The group detects the gap via nonce propagation and requests the prompt through intermediaries. If the user refuses within R_prompt rounds, hosts stop signing.
- Host cannot pretend it didn't receive data. The group will deliver it via relay. If the host still doesn't compute, it's clearly at fault.

#### User creates StartInference but doesn't provide data to host_i

Covered by the recovery protocol above. This is the "user withheld prompt data" cause. Nonce propagation detects the gap, MsgRequestPrompt forces the user to provide data or face hosts refusing to sign.

#### User sends request to host_i but doesn't record StartInference

Not possible. host_i checks the diffs and rejects requests without a corresponding MsgStartInference. No StartInference = no payment authorization = no reason to compute.

> Note: inference validation in the subnet uses the same mechanism as on mainnet. Prompt and response hashes are recorded in subnet state and validated probabilistically at settlement.

## Settlement

User submits `MsgSettleEscrow(state_hash, signatures, missed, invalid)` to mainnet. Mainnet verifies 2/3+ slot-weighted signatures over the state hash. If valid, settlement enters a dispute window of X blocks (TBD).

> Note: the list of individual signatures can be replaced with an aggregated BLS signature in the future to reduce tx size.

During the dispute window, any host can submit a competing state with a higher nonce and 2/3+ signatures. If such a state exists, the user submitted stale state: all remaining escrow goes to hosts as penalty. If no competing state appears within X blocks, settlement finalizes: hosts are paid per token from escrow, unused balance is refunded to the user.

**Unsettled transactions at settlement time.** When the user wants to close the session, there may be in-flight inferences (MsgStartInference without MsgFinishInference) and recent nonces without enough signatures. Two options:

- Flush round: user sends empty requests (no inference, just diffs) in round-robin to collect remaining signatures and let hosts attach pending MsgFinishInference. Produces a fully-settled state before submitting to mainnet. Clean but requires one extra round.
- Skip flush: user settles with whatever state is available. Any in-flight inference cost is forfeited (charged from escrow but hosts keep it even if work wasn't completed). Simpler but user overpays for unfinished work.

TODO: decide which approach. Flush round is more consistent with the rest of the design. Could make it optional:if user skips it, they forfeit the unsettled portion.

**User disappears.** Any group member can submit MsgSettleEscrow after a timeout. All hosts have full state within one round (propagated via diffs). If a host is missing recent state, it can request it from other hosts via the public API endpoint. Same 2/3+ signature requirement, same dispute window. TODO: define timeout trigger (wall-clock from last nonce vs escrow expiry height at creation).

**Inflated state.** User claims less usage than actually happened (to get a larger refund). Requires 2/3+ host signatures over the false state. Reduces to BFT assumption: safe as long as <1/3 of slot-weighted hosts are malicious.

## Example requests

Third request in the happy path (sent to h3). Carries all accumulated diffs with signatures collected so far.

```
POST /chat/completions
Host: h3

{
  "model": "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8",
  "stream": true,
  "messages": [
    {"role": "user", "content": "Write a haiku about Seattle."}
  ],
  "diffs": [
    {"nonce": 1, "txs": ["MsgStartInference(1)"], "sigs": ["sig_h1"]},
    {"nonce": 2, "txs": ["MsgStartInference(2)"], "sigs": ["sig_h2"]},
    {"nonce": 3, "txs": ["MsgFinishInference(1)", "MsgStartInference(3)"], "sigs": []}
  ],
  "state_hash": "<SHA256>"
}
```

For comparison, the first request (to h1) carries only one diff:

```
{
  ...
  "diffs": [
    {"nonce": 1, "txs": ["MsgStartInference(1)"], "sigs": []}
  ],
  "state_hash": "<SHA256>"
}
```

Each diff is a block at a given nonce. Signatures for earlier nonces accumulate over time as hosts return them. By the 3rd request, sig_h1 (returned with req1 response) and sig_h2 (returned with req2 response) are attached to their respective nonces. Nonce 3 has no signatures yet: h3 will sign it and return sig_h3 in the response.


## Weights in subnet

Subnet group formation reuses the slot sampling mechanism from PoC validation (see [proposals/poc/optimize.md](../poc/optimize.md)).

Slot assignment is a deterministic function of (app_hash after escrow creation, escrow_id, validator_weights) using the same `GetSlotsFromSorted` algorithm as in PoC. The chain does not need to compute it at escrow creation. Anyone can derive the group independently. The chain only verifies the group was correct at settlement time (MsgSettleEscrow).

Each slot maps to a host. If a host is sampled into 3 slots, it has weight 3 in the subnet. Each slot carries weight 1. This preserves the mainnet weight distribution inside the subnet without requiring any additional weight tracking.

The slot sequence also defines the round-robin order for user requests.

Requirements for slot count are less strict than in PoC. In PoC, slots protect against adversarial validation (fake participant attacks). In the subnet, the group only needs enough redundancy for availability and settlement signatures. The exact slot count (64 vs 128) is TBD.

TODO: define settlement signature threshold relative to slot count
