# Proposal: Duplicate Nonce Attack Mitigation

This proposal addresses a vulnerability in the PoC v2 off-chain artifact system where a malicious participant could inflate their score by duplicating valid artifacts instead of performing honest computation.

## Problem

In PoC v2, participants generate artifacts off-chain and commit only a Merkle root + count to the chain. Validators sample a small random subset (e.g., 200 out of 100,000) to verify. The current MMR (Merkle Mountain Range) does not enforce any relationship between an artifact's position (leaf index) and its nonce.

**Attack mechanism:**

1. Generate fewer legitimate `(nonce, vector)` pairs than claimed (e.g., 90,000 instead of 100,000)
2. Duplicate some pairs to fill the remaining slots
3. Commit `(root_hash, 100000)` to chain
4. Serve valid proofs when validators request artifacts

Validators have no way to detect that the same `(nonce, vector)` exists at multiple indices unless they happen to sample both.

### Why Duplicates Are Hard to Detect

For comparison, consider invalid artifacts (wrong computation). If a validator samples any invalid artifact, it fails verification immediately. Detection probability follows:

```
P(invalid detected) = 1 - (1 - rate)^k
```

| Fraud Rate | Invalid Artifact Detection | Duplicate Detection |
|:-----------|:---------------------------|:--------------------|
| 1% | 86.6% | 0.2% |
| 10% | ~100% | 2.0% |
| 50% | ~100% | 9.5% |

Invalid artifacts are detected independently when sampled. Duplicates require sampling *both* copies of the same nonce — a collision event with probability proportional to `k²/N`, not `k/N`. This makes duplicate detection fundamentally harder and motivates structural prevention.

### Duplicate Detection Probability

For N=100,000 artifacts with sample size k=200, probability that a single validator's sample contains at least one duplicate pair:

| Duplicate Rate | Duplicate Pairs | Detection Probability |
|:---------------|:----------------|:----------------------|
| 10% | 5,000 | ~2.0% |
| 20% | 10,000 | ~3.9% |
| 30% | 15,000 | ~5.8% |
| 50% | 25,000 | ~9.5% |

Even with 50% of artifacts being duplicates, a single validator has less than 10% chance of detecting fraud.

### Impact of Population Size

The attack becomes more effective with larger artifact counts:

| Total Artifacts | 10% Duplicates | 20% Duplicates | 30% Duplicates | 50% Duplicates |
|:----------------|:---------------|:---------------|:---------------|:---------------|
| 1,000 | 86.4% | 98.1% | 99.7% | 100.0% |
| 5,000 | 32.8% | 54.9% | 69.7% | 86.3% |
| 10,000 | 18.0% | 32.8% | 45.0% | 63.0% |
| 100,000 | 2.0% | 3.9% | 5.8% | 9.5% |
| 1,000,000 | 0.2% | 0.4% | 0.6% | 1.0% |

With large artifact counts (100k+), detection probability drops below 10% even for aggressive duplication.

### Multi-Validator Aggregation

With multiple validators, detection improves but remains limited:

```
P(at least one detects) = 1 - (1 - P_single)^V
```

For 100k artifacts with 10% duplicates and 10 validators, where `P_single = 2.0%` from the table above:

```
P = 1 - (1 - 0.02)^10 ≈ 18%
```

Still an 82% chance of the attack going undetected.

### Non-Response Evasion

Even the low detection probability overstates the risk to attackers. A dishonest participant can evade detection entirely:

1. Receive proof request for leaf index `I`
2. Check if index `I` corresponds to a duplicated nonce
3. If yes, refuse to respond (timeout)

From the validator's perspective, this is indistinguishable from legitimate network issues. Without aggressive non-response penalties (which would punish honest nodes with connectivity problems), attackers can selectively hide evidence of duplication.

This further motivates structural prevention over detection-based approaches.

## Proposal

**Goal**: Enforce `position == nonce` in the commitment structure. If each nonce can only occupy one specific slot, duplicates become impossible by design.

Approach: use a **Sparse Merkle Sum Tree (SMST)** where nonce determines the tree path.

### Constraints

- Commits every 5 seconds during generation
- Generation phase: ~5 minutes, ~5M artifacts
- Max nonce: ~15M (tree depth D=24). Larger values must work but we optimize for this scale.
- Validation happens after generation completes

### SMST Overview

**Structure**: Fixed-depth binary tree where nonce bits determine the path. Empty subtrees use precomputed "empty hash".

**Sum property**: Each node stores `count = left.count + right.count`. Enables sampling dense index `[0, total_count)` in sparse tree:

```
At each node:
  if index < left.count: go left
  else: go right, index -= left.count
```

**Why duplicates are impossible**: Position determined by nonce value. One slot per nonce. Inserting same nonce overwrites, doesn't duplicate.

### Option A: SMST During Generation

Maintain SMST incrementally. Commit root every 5 seconds.

- Pro: Single structure, duplicates impossible at all times
- Con: Insert O(24) vs MMR O(1), needs snapshot handling for historical commits

Q: Is O(24) insert overhead acceptable? Estimated ~100ms per 5-sec window at 83K inserts.
A: If confirmed - seems like nothing

### Option B: MMR + Post-Generation SMST (Recommended)

Keep current MMR during generation. Build SMST after generation completes.

```
Generation:     artifact --> append to MMR --> (every 5 sec) commit (mmr_root, count)
Post-gen:       load final MMR artifacts --> build SMST --> publish (smst_root, count)
Validation:     sample from SMST --> return artifact + proof
```

**Critical constraint**: SMST must contain exactly the artifacts from the final MMR commit. Otherwise attacker could mask duplicates by adding legitimate nonces after last commit.

### Equivalence Proof

Simple count comparison is insufficient:
- Attacker has 100K in MMR (5K duplicates = 95K unique)
- Adds 5K legitimate nonces after last MMR commit
- SMST has 100K unique, counts match, fraud undetected

**Possible solutions**:
1. Cryptographic binding: SMST leaves include MMR leaf index
2. Block height cutoff enforced in SMST construction
3. Commit SMST alongside final MMR commit

Q: What's the minimal proof that SMST contains exactly the MMR artifacts?

### Complexity

| Operation | Cost |
|:----------|:-----|
| Generation (MMR append) | O(1) per artifact |
| Post-gen SMST build (5M) | 5M * 24 = 120M hashes, ~6s |
| Proof generation | O(24) |
