package host

import (
	"context"
	"fmt"

	"subnet/types"
)

// ExecutorClient contacts the executor host to check inference status.
type ExecutorClient interface {
	GetMempool(ctx context.Context) ([]*types.SubnetTx, error)
	Send(ctx context.Context, req HostRequest) (*HostResponse, error)
}

// VerifyRefusedTimeout checks if a refused timeout is valid.
//
// Flow:
//  1. Check local state: inference must be pending (no receipt).
//  2. Check local mempool for MsgConfirmStart -- if found, reject.
//  3. Contact executor, forward prompt data via Send.
//  4. If executor responds with receipt -> reject (executor got data, should compute).
//  5. If executor unreachable -> accept.
func VerifyRefusedTimeout(
	ctx context.Context,
	st types.EscrowState,
	inferenceID uint64,
	promptData []byte,
	localMempool []*types.SubnetTx,
	executorClient ExecutorClient,
	config types.SessionConfig,
	nowUnix int64,
) (bool, error) {
	rec, ok := st.Inferences[inferenceID]
	if !ok {
		return false, fmt.Errorf("inference %d not found", inferenceID)
	}
	if rec.Status != types.StatusPending {
		return false, fmt.Errorf("inference %d: expected pending, got %d", inferenceID, rec.Status)
	}

	// Reject if refusal timeout deadline has not passed.
	if nowUnix-rec.StartedAt < config.RefusalTimeout {
		return false, nil
	}

	// Fast path: check local mempool for MsgConfirmStart.
	for _, tx := range localMempool {
		if cs := tx.GetConfirmStart(); cs != nil && cs.InferenceId == inferenceID {
			return false, nil // executor already confirmed
		}
	}

	// Contact executor.
	if executorClient != nil {
		// Try to get the executor's mempool.
		executorMempool, err := executorClient.GetMempool(ctx)
		if err == nil {
			// Check if executor has a confirm start.
			for _, tx := range executorMempool {
				if cs := tx.GetConfirmStart(); cs != nil && cs.InferenceId == inferenceID {
					return false, nil
				}
			}
		}
		// err != nil means executor unreachable, which supports the timeout claim.
	}

	return true, nil
}

// VerifyExecutionTimeout checks if an execution timeout is valid.
//
// Flow:
//  1. Check local state: inference must be started (has receipt, no finish).
//  2. Check local mempool for MsgFinishInference -- if found, reject.
//  3. Contact executor, check if it has MsgFinishInference in its mempool.
//  4. If executor has result -> reject (finish should be included instead).
//  5. If executor unreachable or no result -> accept.
func VerifyExecutionTimeout(
	ctx context.Context,
	st types.EscrowState,
	inferenceID uint64,
	localMempool []*types.SubnetTx,
	executorClient ExecutorClient,
	config types.SessionConfig,
	nowUnix int64,
) (bool, error) {
	rec, ok := st.Inferences[inferenceID]
	if !ok {
		return false, fmt.Errorf("inference %d not found", inferenceID)
	}
	if rec.Status != types.StatusStarted {
		return false, fmt.Errorf("inference %d: expected started, got %d", inferenceID, rec.Status)
	}

	// Reject if execution timeout deadline has not passed.
	if nowUnix-rec.StartedAt < config.ExecutionTimeout {
		return false, nil
	}

	// Fast path: check local mempool for MsgFinishInference.
	for _, tx := range localMempool {
		if fi := tx.GetFinishInference(); fi != nil && fi.InferenceId == inferenceID {
			return false, nil // executor already finished
		}
	}

	// Contact executor.
	if executorClient != nil {
		executorMempool, err := executorClient.GetMempool(ctx)
		if err == nil {
			for _, tx := range executorMempool {
				if fi := tx.GetFinishInference(); fi != nil && fi.InferenceId == inferenceID {
					return false, nil // executor has the finish, reject timeout
				}
			}
		}
		// err != nil means executor unreachable, which supports the timeout claim.
	}

	return true, nil
}
