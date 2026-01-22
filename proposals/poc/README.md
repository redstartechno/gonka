# Proposal: PoC v2 & Offchain PoC data

This proposal describes the current status of the PoC v2 migration, which integrates the PoC procedure into vLLM and uses `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8` model.

The PoC v2 initiative addresses two key objectives:

1. Integrate the PoC procedure directly into vLLM, enabling an immediate switch from inference to PoC without offloading the model or loading a separate PoC model, so PoC can be triggered quickly with minimal phase-switch overhead and no dedicated setup.

2. Migrate artifact storage off-chain using Merkle commitments (only the root hash + count is recorded on-chain) to reduce on-chain data volume.

These changes maintain robustness against minimal model changes (e.g., quantization). In the current vLLM integration, inference and PoC can't be executed in the same forward pass, but PoC can be scheduled in the next forward pass.

Keeping PoC inside vLLM also makes it straightforward to integrate PoC into the chat completion path next. In such setup, inference and PoC can run in the same batch and the same forward pass.

## vLLM

The current version of PoC integrated with vLLM: https://github.com/gonka-ai/vllm/tree/gm/poc-layers

Instead of offloading the inference model and loading a randomly initialized model for PoC, the integrated approach applies block-dependent randomization to specific layers:

- Input: bypass token embeddings by feeding deterministic random `inputs_embeds` seeded by `(block_hash, public_key, nonce)`.
- Hidden layers: apply per-layer Householder transforms via forward hooks seeded by `(block_hash, layer_idx)`, active only during PoC forward context.
- Output: from the last-token hidden state, normalize, select `k_dim` indices per nonce, and apply a deterministic Haar-like rotation (Householder chain) seeded by `(block_hash, public_key, nonce)`.
- Artifact: output is a `k_dim` vector (default `k_dim=12`, FP16, base64-encoded). In our experiments, increasing `k_dim` beyond 12 did not improve fraud detection quality, while lower values led to degradation.

### Experiment

We compare honest outputs across different hardware generations (H100, B200, A100) and model variants (FP8 vs INT4-quantized) to measure expected divergence. We also compare against a modified model on the same hardware (closest case for fraud detection). Validation uses an L2-distance mismatch rule with a calibrated threshold and a statistical test.

Report: [report.md](./PoC_V2_Validation_Report.md)


## Merkle Tree (MMR, Off-chain Artifacts)

PoC v2 artifacts are larger than v1, and we need enough total nonces per participant to allow meaningful sampling without pushing large payloads on-chain. The approach is to keep artifacts off-chain and store only an on-chain commit: `(root_hash, count)` (MMR root + leaf count).

Validators:
- Query the chain for all participants’ latest commits for the stage (`root_hash`, `count`) and validate each participant against their commit.
- Sample `leaf_index` values in `[0, count)`
- Fetch `(artifact, proof)` from the participant API (`POST /v1/poc/proofs`) and verify against the committed `root_hash`.
- Run statistical validation on the verified artifacts and submit results in batch (one `MsgSubmitPocValidationsV2` can include validations for multiple participants at the same stage height).

```
┌────────────────────────────────────────────────────────────────────────┐
│ Generation Phase                                                       │
│   Participant generates artifacts → stores locally in MMR              │
│   Periodically submits MsgPoCV2StoreCommit(root_hash, count) to chain  │
└────────────────────────────────────────────────────────────────────────┘
         ↓
┌────────────────────────────────────────────────────────────────────────┐
│ After Generation Phase                                                 │
│   Submits MsgMLNodeWeightDistribution (weight per MLNode)              │
└────────────────────────────────────────────────────────────────────────┘
         ↓
┌────────────────────────────────────────────────────────────────────────┐
│ Validation Phase                                                       │
│   Validator queries chain for commits (root_hash, count)               │
│   Samples leaf_indices in [0, count) deterministically                 │
│   Requests artifacts + proofs from participant API (/v1/poc/proofs)    │
│   Verifies proofs → statistical validation → submits MsgSubmitPoc...V2 │
└────────────────────────────────────────────────────────────────────────┘
```

## Migration

PoC v2 rollout requires coordinated updates across:
- `mlnode`: new version with V2 endpoints + switching to `Qwen/Qwen3-235B-A22B-Instruct-2507-FP8`
- `api`
- `node` (chain)

`mlnode` can be upgraded independently (before or after the `api`/`node` upgrade), but must be upgraded before PoC v2 is enabled. It is backward compatible and can keep running PoC v1 while the network is still in v1 mode.

`api` and `node` are updated together via a single on-chain upgrade. That upgrade enables the end-to-end PoC v2 flow.

If issues arise during migration, `api` fixes can be deployed off-chain without requiring a governance vote, as long as they don't change protocol behavior.

Because of this coordination requirement, the migration keeps PoC v1 and PoC v2 available at the same time. During migration, PoC v1 remains the primary source of weight. PoC v2 can be enabled for Confirmation PoC only, so results are recorded on-chain for tracking but not applied to main PoC weight/slashing. Optionally, V2 can be enabled for only a fraction of Confirmation PoC events (e.g., 1/3) for gradual rollout. Once a clear majority of nodes reliably passes PoC v2, a governance proposal switches the primary PoC to v2.

The V1/V2 switch is controlled by a governance param:
- `poc_v2_enabled` (false = PoC v1, true = PoC v2)

Setting it to `true` requires no software upgrade - just a governance proposal.

`confirmation_poc_v2_enabled` exists in params but is not wired into `api`/`node` logic yet. Once implemented, it will enable the gradual rollout described above.


## Status

Current version of code passes testermint tests for both PoC v1 and PoC v2 

- [testing, separate branch]: enable PoC v2 for Confirmation PoC only
- [todo]: weight validation during the epoch when PoC v2 becomes main source of weight
- [need review]: sending batches to validate for PoC v1 and PoC v2
- [need review]: cancelling existing inference requests at mlnode when PoC starts
- [need review]: MMR behaviour when > 1000 MLNodes
- [to define]: PoC phase length for PoC v2