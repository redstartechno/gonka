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

**Example: 3 inference requests**

```
User -> Mainnet:  MsgCreateEscrow(amount=100GNK)
                  <- escrow_id=42, group=[host1, host2, host3, ...]

User -> host1:    POST /chat/completions  (req 1, diffs=[])
                  <- streamed response + sign(state after MsgStartInference(1))

User -> host2:    POST /chat/completions  (req 2, diffs=[
                    {txs:[MsgStartInference(1)], sigs:[h1_sig]},
                    {txs:[MsgFinishInference(1)]}
                  ])
                  <- streamed response + sign(state after MsgStartInference(2))

User -> host3:    POST /chat/completions  (req 3, diffs=[
                    {txs:[MsgStartInference(2)], sigs:[h2_sig]},
                    {txs:[MsgFinishInference(2)]}
                  ])
                  <- streamed response + sign(state after MsgStartInference(3))

...

User -> Mainnet:  MsgSettleEscrow(finalState, signatures=[h1_sig, h2_sig, ...], missed, invalid)
                  <- user refund + hosts paid
```

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


### Sub-Chain Protocol

Let's focus on the subnet logic. Essentially, it's some sort of shard and might be considered as own blockchain with voting weight if fully provided by mainnet and then it's settled on mainnet when "session" is finished.

To make it more lightweight, parallizable and enforce user to use all hosts from the group for inferece requests, we introduce new assumption "per user state".

What user wants to do?
Send openapi-compartible REST API requests like `/chat/completions`, `/embeddings`, etc. And now as less as possible about the blockchain 

What chain wants (and essentiallly what we tried to achive on mainnet but less successfull that we would want to have):
- chain knows when request is started and finished => another hosts can measure performance of request to compare with expected performance and punish if some executor works much worse then expected (essentailly missed rate now)
- chain knows the initial hash of prompt and hash of final payload with signatures of user (for hash of prompt) and executor (for hash of payload). signatures of prompt is used to authorize payment, signature of payload if used for probabilistic inference validation (if invalid => invalidation rate punishment)
- chain enforces distribution of requests between executors proportionally to their weight

And as already mention - chain doesn't want to have all these data to be processed on mainnet

----

Key points of the idea:

- data is saved per user independely. for example, there is a chain of base diffs for the specific user / its transaction.state is updated based on such state 
  => it's possible to parallize processors by user. e.g. load balancer routes request from the user with address A to group of nodes which has access to DB with it's data. single state is not needed even inside the shard

- it's mainly responsibility of the user to propagate transaction (both created by the user itself and initiated from nodes at response)

- user attach diffs data in inference requests it makes 
  - gossiping might still be needed as it requires full round to propagate transaction. how to effectively propagate is open question

- signature of state(hieight) should be returned immediately after request
  - if `/chat/completions` request is sent host1, it essentially user creates `MsgStartInference(1)` task at host1. if host1 is honest, it must immediatly return `(state, signature)`, without waiting for execution of request. it then will return sign `MsgFinishInference(1)` and user will propagate it to the network (when it'll be in state of that user).

- user must iterate hosts in group by pre-defined order. it naturally distribute requests accross hosts (not the amount of real work but at least requests). It attach nonce to each transaction (or each diff) => it can't skip
  - if some host if not available, user keeps to propagate diffs to the next host. as it's essentially propagates ALL diffs in the current round (to cover all hosts), it'll attach diff without signature too. another host will follow protocol to decide together if host must be punished or not in that case
  - ideally, locks must be only on nonces producing and new messages from the host itself. we can't rely in waiting for response + signature from hosts => it might be additional optimistic delay in getting data. if to wait for at least some request result => it's wont allow to send requests fast




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


### Weights in sub-chain


-----

InferenceGroup:
