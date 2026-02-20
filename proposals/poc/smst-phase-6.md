# Phase 6: Security Verification

Verified SMST implementation provides cryptographic guarantees against duplicate attacks.

## Security Properties

### Duplicate Prevention

The SMST structure inherently prevents duplicates by using the nonce as the path determinant.

- `Insert(nonce, ...)` checks `hasNonce` map for immediate duplicate rejection
- Structurally, a nonce `N` maps to a unique path `P`. It is impossible to store two different values for nonce `N` in the same tree
- Verification `VerifySMSTProofWithCounts` reconstructs the path based on the claimed nonce. If an attacker tries to store nonce `N` at path `P'` (to duplicate it), the verification will fail because it will attempt to traverse path `P`

### Count Inflation Prevention

The attack where a malicious participant claims a higher count (e.g., 100) than actual artifacts (e.g., 50) is prevented by the Merkle Sum Tree properties.

1. Count Commitment: The root hash includes the total `count` of leaves via `H_root = Hash(left, right, count)`. To claim `Count=100`, the attacker must provide a root hash that corresponds to a tree with 100 items.

2. Index Binding: The verifier requests an artifact at a specific dense index `i`. `VerifySMSTProofWithDenseIndex` verifies that the provided proof actually leads to the `i`-th non-empty leaf by summing the counts of left siblings along the path.

3. Since sibling counts are committed in the node hashes, the attacker cannot forge the index without changing the root hash.

### Verified Index Logic

The function `VerifySMSTProofWithDenseIndex` in `smst_verify.go` implements the critical check:

```go
computedIndex := uint32(0)
for i := 0; i < depth; i++ {
    if path[i] {
        computedIndex += elements[i].SiblingCount
    }
}
return computedIndex == denseIndex
```

This ensures that if the validator asks for the 90th artifact, the proof must correspond to the 90th artifact in the tree committed to by `root_hash`.

## Hardening Tests

Added explicit attack scenario tests in `smst_test.go`:

| Test | What it verifies |
|:-----|:-----------------|
| `TestSMSTCountInflationAttackFails` | Claiming higher count than actual leaves fails |
| `TestSMSTProofWithWrongCountFails` | count-1, count+1 variations all fail verification |
| `TestSMSTNonceHasUniqueDenseIndex` | Each nonce maps to exactly ONE dense index |
| `TestSMSTNegativeNonces` | Negative int32 nonces handled correctly (depth expands to 32) |
| `TestSMSTCountBoundaries` | Edge cases: count=0, denseIndex >= count, single leaf |

These tests document the security properties and provide regression protection.

## Verification

All 44 SMST tests pass including 5 new security hardening tests.

## Conclusion

The SMST implementation provides cryptographic guarantees against:

- Duplicate Nonces: Structural impossibility (one slot per nonce)
- Count Inflation: Detected by sibling count commitment in root hash
- Index Swapping: Detected by path-to-nonce verification
