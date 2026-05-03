# Upgrade Proposal: v0.2.13

This document outlines the proposed changes for on-chain software upgrade v0.2.13.
The `Changes` section details the major modifications, and the `Upgrade Plan` section describes the process for applying these changes.

## Upgrade Plan

This PR updates the code for the `api` and `node` services. The PR modifies the container versions in `deploy/join/docker-compose.yml`.

The binary versions will be updated via an on-chain upgrade proposal. For more information on the upgrade process, refer to [`/docs/upgrades.md`](https://github.com/gonka-ai/gonka/blob/upgrade-v0.2.13/docs/upgrades.md).

Existing hosts are **not** required to upgrade their `api` and `node` containers. The updated container versions are intended for new hosts who join after the on-chain upgrade is complete.

## Proposed Process

1. Active hosts review this proposal on GitHub.
2. If the on-chain proposal is approved, this PR will be merged immediately after the upgrade is executed on-chain.

## Testing

The on-chain upgrade from version `v0.2.12` to `v0.2.13` has been successfully deployed and verified on the testnet. No regression in core functionality or performance has been observed during testing. More testing will be executed leading up to the upgrade.

Reviewers are encouraged to request access to testnet environments to validate both node behavior and the on-chain upgrade process, or to replay the upgrade on private testnets.

## Migration

The on-chain migration logic is defined in [`upgrades.go`](https://github.com/gonka-ai/gonka/blob/upgrade-v0.2.13/inference-chain/app/upgrades/v0_2_13/upgrades.go).

Migrations:
- Sets `DevshardEscrowParams.MaxEscrowsPerEpoch` to `500_000`.

## Changes

### [PR #TBD](https://github.com/gonka-ai/gonka/pull/TBD) Title
- Short description of the change.

### Other changes
- [PR #TBD](https://github.com/gonka-ai/gonka/pull/TBD) Title.

## Proposed Bounties

Bounty ID | Sum GNK | Bounty Explanation | GitHub ID
-- | -- | -- | --
v0.2.13 | TBD | release management | TBD
v0.2.12 | TBD | upgrade review | TBD
