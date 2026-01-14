package mlnodeclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"

	"decentralized-api/utils"
)

// PoC v2 offchain init endpoint.
// Note: the base URL (including any "/api" prefix) comes from broker Node.PoCUrlWithVersion(...).
const PoCV2InitGeneratePath = "/api/v1/inference/pow/init/generate"
const PoCV2StopPath = "/api/v1/inference/pow/stop"

// PoCParamsV2 mirrors the mlnode PoC v2 params model schema.
type PoCParamsV2 struct {
	Model  string `json:"model"`
	SeqLen int    `json:"seq_len"`
	// KDim   int    `json:"k_dim"`
}

// PoCInitGenerateRequestV2 is the JSON body for mlnode `POST /init/generate` for PoC v2.
type PoCInitGenerateRequestV2 struct {
	BlockHash   string `json:"block_hash"`
	BlockHeight int64  `json:"block_height"`
	PublicKey   string `json:"public_key"`

	NodeID    int `json:"node_id"`
	NodeCount int `json:"node_count"`

	Params PoCParamsV2 `json:"params"`

	URL string `json:"url,omitempty"`
	// BatchSize int `json:"batch_size,omitempty"`
	// batch_size is intentionally omitted - MLNode will use its default
}

// PoCStatusResponseV2 represents the response from /api/v1/inference/pow/status.
type PoCStatusResponseV2 struct {
	Status   string            `json:"status"` // "IDLE", "GENERATING", "MIXED", "NO_BACKENDS"
	Backends []BackendStatusV2 `json:"backends,omitempty"`
}

// BackendStatusV2 represents the status of a single vLLM backend.
type BackendStatusV2 struct {
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// PoCInitGenerateResponseV2 represents the response from /api/v1/inference/pow/init/generate.
type PoCInitGenerateResponseV2 struct {
	Status   string          `json:"status"`
	Backends int             `json:"backends,omitempty"`
	NGroups  int             `json:"n_groups,omitempty"`
	Results  []BackendResult `json:"results,omitempty"`
	Errors   []BackendError  `json:"errors,omitempty"`
}

// BackendResult represents a successful backend response.
type BackendResult struct {
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// BackendError represents a failed backend response.
type BackendError struct {
	Port  int    `json:"port"`
	Error string `json:"error"`
}

// PoCStopResponseV2 represents the response from /api/v1/inference/pow/stop.
type PoCStopResponseV2 struct {
	Status  string          `json:"status"`
	Results []BackendResult `json:"results,omitempty"`
	Errors  []BackendError  `json:"errors,omitempty"`
}

// InitGenerateV2 starts PoC v2 generation on the MLNode.
// This is the entry point for mining - it starts generation and artifacts are delivered via callback.
func (c *Client) InitGenerateV2(ctx context.Context, req PoCInitGenerateRequestV2) (*PoCInitGenerateResponseV2, error) {
	requestUrl, err := url.JoinPath(c.pocUrl, PoCV2InitGeneratePath)
	if err != nil {
		return nil, err
	}

	httpResp, err := utils.SendPostJsonRequest(ctx, &c.client, requestUrl, req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}

	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("InitGenerateV2 failed with status %d: %s", httpResp.StatusCode, string(body))
	}

	var resp PoCInitGenerateResponseV2
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// StopPowV2 stops PoC v2 generation on the MLNode.
// It is intended to be safe to call multiple times (mlnode should treat it as idempotent).
func (c *Client) StopPowV2(ctx context.Context) (*PoCStopResponseV2, error) {
	requestUrl, err := url.JoinPath(c.pocUrl, PoCV2StopPath)
	if err != nil {
		return nil, err
	}

	httpResp, err := utils.SendPostJsonRequest(ctx, &c.client, requestUrl, nil)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, err
	}
	if httpResp.StatusCode >= 400 {
		return nil, fmt.Errorf("StopPowV2 failed with status %d: %s", httpResp.StatusCode, string(body))
	}

	var resp PoCStopResponseV2
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
