# Inference Scaling 

## Problem 

Now per each inference next transactions are recorded on-chain:
- MsgStartInference
- MsgFinishInference
- MsgValidation (it can be from 0 to N_hosts transaction, current document consider 1 per inference)

3 txs per inference. Max capacity per block is ~5000
=> 5000 / 3 = 1666 inferences per block 
=> 1666 / 6 = 277 inferences per sec

Let's consider 4xH100 with deployed Qwen235B.
For 5000/1000 input/output tokens, such model can process 3.5-4 RPs (TODO: confirm)
=> 277 / 3.5 * 4 = 316 H100 GPU

Probably different requests can be backed together in single transaction.
Even if achieve 100x optimization with such approach, handling per request on chain billing is not scalable to hunderds thousands 

The situation becomes better with longer requests (more compute used, less billing info per unit of compute)
And much worse with smaller model (which are required for some domains)



> Note: in practice, the main limit is not even amount of transactions in blocks but even the computation per these 3 transaction.
Seems like it's becoming a problem now if there are more then couple hunderds such transactions per block. 
Based on profiling data it can be optimized significantly (2-10 times), but this limitation will still hit us first


## Proposal

This proposal describe approach which moves all per-inference communication off-chain. 
Chain will process only initial transaction to put coins in escrow and assign subgroup of hosts which will handle execute inferences. 
All communication around inferences and their validations will happen inside the subgroup directly during quite long period of time (e.g. epoch).
Then, user (or some of hosts) would have to settle the escrow, submitting final state of usage for the user signed by majority of hosts in such subgroup.
After such transaction submitted, user get's what left from escrow, participants are getting paid. 

Effectively, as each subgroup would have to achieve consensus for the final state, the architecture will consist of:
- main blockchain
- many sub-chains / shards with extremely lightweight architecture 

Sub-chains will be able to process only the inference related transactions and their decision might affect only the escrows, assigned to such sub-chains

### User Flow

- [mainchain]: user creates `MsgCreateEscrow(100GNK)` 
- [subchain]: user interact with hosts in subgroup in pre-defined order
- [mainnet]: at the end of session, user creates `MsgSettleEscrow(finalState, signatures, missed, invalid)`

Q1: How validation / invalidation stats from subchain is settled? Same `MsgSettleEscrow` or smth else. I'd consider separate to settle only decisions / punishments (they might be included also?)
Q2: Do Hosts have per group state, not per participant? 

A: Seems like it's better to start with full decisions on main chain, to avoid global (not per used) state at each Host. But that can be decision inside the subgroup later



### Main Network Protocol

```
MsgCreateEscrow(
  creatorAddr
  amount,
)
```
1. move money to escrow
2. return id to sample N(64?) slots-hosts (same design as optimize.md) 

```
MsgSettleEscrow(
  creatorAddr, # can be both user or someone from group
  finalState,
  signatures,
  missed,
  invalid
)
```

### Sub-Chain Protocol

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




host_1, ..., host_N
weight(host_i)

top-K hosts are validators of main chain (as currently)

new1: no StartInference, FinishInference, Inference Validation on main chain

new2: per user chain, to use inference user must send tx OpenInferenceGroup(amount)
  gets group_id 

Group - group of host with identical weight, samlped from host in slot-style way (see: optimize.md). Size of group is N


-----

InferenceGroup:
