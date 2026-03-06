# Subnet Attack Vectors

## Executor refuses to work

Executor receives MsgStartInference but never signs a receipt.

After RefusalTimeout, verifiers challenge the executor via ChallengeReceipt. If the executor is unreachable or returns no receipt, verifiers vote accept. The user gets a full refund; the executor gets missed++ in HostStats.

ChallengeReceipt also handles the case where the executor is alive but the user never delivered the prompt. The verifier forwards diffs and payload through ChallengeReceipt, forcing the executor to either produce a receipt (and compute) or be marked unresponsive.

## User withholds or corrupts prompt

The user sends MsgStartInference but never delivers the prompt, or sends a prompt that doesn't match the committed prompt_hash.

Verifiers run VerifyPayload before forwarding to the executor. Mismatched prompt_hash, model, input_length, max_tokens, or started_at causes the verifier to reject the timeout. Corrupt payloads never reach vote threshold.

If verifiers also lack the payload (user never sent it to anyone), they reject the timeout because VerifyRefusedTimeout requires a valid payload. No payload means no timeout votes.

## User ignores MsgFinishInference

User receives the executor's MsgFinishInference but excludes it from diffs, then tries to claim timeout to avoid paying.

Timeout requires >voteThreshold signed accept votes. Hosts contact the executor during verification -- if the executor has the finish in its mempool, they refuse. The user can't suppress the executor's evidence from other hosts.

After `grace` nonces without inclusion, the executor withholds its state signature. Without 2/3+ signatures the user can't settle. Grace is nonce-based (not time-based) because the user is the sequencer -- wall-clock time would let the user delay requests to game the window.

## StartedAt manipulation

User sends MsgStartInference with StartedAt=0. Timeout deadlines are computed as nowUnix - base >= timeout. With a past base both deadlines are immediately satisfied.

Refused timeout is safe: ChallengeReceipt delivers data to the executor, the executor produces a receipt, timeout rejected. Execution timeout is vulnerable: the executor just started computing, has no MsgFinishInference in its mempool yet, verifiers see the deadline passed and accept the timeout. User gets a full refund for work already started.

Execution timeout uses confirmed_at as its deadline base instead of started_at. The executor sets confirmed_at to its wall-clock time when signing the ExecutorReceiptContent. The signature covers this value, so the user cannot alter it. VerifyExecutionTimeout computes the deadline from confirmed_at, which the executor controls. Refused timeout still uses started_at because ChallengeReceipt makes it non-exploitable regardless of the base.
