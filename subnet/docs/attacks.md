# Subnet Attack Vectors

## User ignores MsgFinishInference

User receives executor's MsgFinishInference but excludes it from diffs,
then tries to submit MsgTimeoutInference to avoid paying.

Mitigations:

1. Timeout requires >voteThreshold signed accept votes from hosts.
   Hosts contact the executor during verification -- if executor has the finish,
   they refuse. User can't suppress executor's evidence from other hosts.

2. After `grace` nonces without inclusion, executor withholds its state signature.
   Without 2/3+ signatures the user can't settle.

3. Grace is nonce-based, not time-based. The user is the sequencer, so the
   staleness counter advances only when the protocol advances. Wall-clock time
   would let the user delay requests to game the window and would break
   determinism across hosts.
