package subnet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"

	"decentralized-api/broker"
	"decentralized-api/chainphase"
	"decentralized-api/completionapi"
	"decentralized-api/payloadstorage"
	"decentralized-api/utils"

	"subnet"
)

// EngineAdapter implements subnet.InferenceEngine by delegating to broker and completionapi.
type EngineAdapter struct {
	broker       *broker.Broker
	nodeVersion  string
	payloadStore payloadstorage.PayloadStorage
	phaseTracker *chainphase.ChainPhaseTracker
	httpClient   *http.Client
}

func NewEngineAdapter(
	b *broker.Broker,
	nodeVersion string,
	ps payloadstorage.PayloadStorage,
	phaseTracker *chainphase.ChainPhaseTracker,
	httpClient *http.Client,
) *EngineAdapter {
	return &EngineAdapter{
		broker:       b,
		nodeVersion:  nodeVersion,
		payloadStore: ps,
		phaseTracker: phaseTracker,
		httpClient:   httpClient,
	}
}

func (e *EngineAdapter) Execute(ctx context.Context, req subnet.ExecuteRequest) (*subnet.ExecuteResult, error) {
	seed := int32(req.InferenceID)

	modified, err := completionapi.ModifyRequestBody(req.Prompt, seed)
	if err != nil {
		return nil, fmt.Errorf("modify request body: %w", err)
	}

	resp, err := broker.DoWithLockedNodeHTTPRetry(e.broker, req.Model, nil, 3,
		func(node *broker.Node) (*http.Response, *broker.ActionError) {
			url := node.InferenceUrlWithVersion(e.nodeVersion) + "/v1/chat/completions"
			httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(modified.NewBody))
			if reqErr != nil {
				return nil, broker.NewApplicationActionError(reqErr)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpResp, postErr := e.httpClient.Do(httpReq)
			if postErr != nil {
				return nil, broker.NewTransportActionError(postErr)
			}
			return httpResp, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("broker execute: %w", err)
	}
	defer resp.Body.Close()

	processor := completionapi.NewExecutorResponseProcessor("")
	if err := completionapi.ProcessHTTPResponse(resp, processor); err != nil {
		return nil, fmt.Errorf("process response: %w", err)
	}

	completionResp, err := processor.GetResponse()
	if err != nil {
		return nil, fmt.Errorf("get completion response: %w", err)
	}

	bodyBytes, err := completionResp.GetBodyBytes()
	if err != nil {
		return nil, fmt.Errorf("get body bytes: %w", err)
	}

	hash := sha256.Sum256(bodyBytes)

	usage, err := completionResp.GetUsage()
	if err != nil {
		return nil, fmt.Errorf("get usage: %w", err)
	}

	canonicalized, err := utils.CanonicalizeJSON(modified.NewBody)
	if err != nil {
		return nil, fmt.Errorf("canonicalize prompt: %w", err)
	}
	promptPayload := []byte(canonicalized)

	storageKey := SubnetPayloadKey(req.EscrowID, req.InferenceID)
	epochID := e.currentEpochID()
	if err := e.payloadStore.Store(ctx, storageKey, epochID, promptPayload, bodyBytes); err != nil {
		return nil, fmt.Errorf("store payloads: %w", err)
	}

	return &subnet.ExecuteResult{
		ResponseHash: hash[:],
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}, nil
}

func (e *EngineAdapter) currentEpochID() uint64 {
	epochState := e.phaseTracker.GetCurrentEpochState()
	if epochState != nil {
		return epochState.LatestEpoch.EpochIndex
	}
	return 0
}

// SubnetPayloadKey creates a namespaced storage key for subnet payloads.
// Format: "subnet:<escrowID>:<inferenceID>" to prevent cross-session collisions.
func SubnetPayloadKey(escrowID string, inferenceID uint64) string {
	return fmt.Sprintf("subnet:%s:%d", escrowID, inferenceID)
}

// SubnetPayloadKeyFromString is like SubnetPayloadKey but takes inferenceID as string.
func SubnetPayloadKeyFromString(escrowID string, inferenceID string) string {
	return fmt.Sprintf("subnet:%s:%s", escrowID, inferenceID)
}

// Compile-time check.
var _ subnet.InferenceEngine = (*EngineAdapter)(nil)
