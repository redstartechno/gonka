package subnet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"decentralized-api/broker"
	"decentralized-api/chainphase"
	"decentralized-api/completionapi"
	"decentralized-api/cosmosclient"
	"decentralized-api/internal/validation"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/cmd/inferenced/cmd"
	"github.com/productscience/inference/x/inference/calculations"

	"subnet"
	"subnet/bridge"
)

// ValidationAdapter implements subnet.ValidationEngine by re-executing inference
// with enforced tokens and comparing logits.
type ValidationAdapter struct {
	broker       *broker.Broker
	nodeVersion  string
	phaseTracker *chainphase.ChainPhaseTracker
	httpClient   *http.Client
	bridge       bridge.MainnetBridge
	recorder     cosmosclient.CosmosMessageClient
}

func NewValidationAdapter(
	b *broker.Broker,
	nodeVersion string,
	phaseTracker *chainphase.ChainPhaseTracker,
	httpClient *http.Client,
	br bridge.MainnetBridge,
	recorder cosmosclient.CosmosMessageClient,
) *ValidationAdapter {
	return &ValidationAdapter{
		broker:       b,
		nodeVersion:  nodeVersion,
		phaseTracker: phaseTracker,
		httpClient:   httpClient,
		bridge:       br,
		recorder:     recorder,
	}
}

func (v *ValidationAdapter) Validate(ctx context.Context, req subnet.ValidateRequest) (*subnet.ValidateResult, error) {
	inferenceID := strconv.FormatUint(req.InferenceID, 10)
	epochID := req.EpochID
	if epochID == 0 {
		epochID = v.currentEpochID()
	}

	// Fetch payloads from executor
	promptPayload, responsePayload, err := v.fetchPayloadsFromExecutor(ctx, req, inferenceID, epochID)
	if err != nil {
		return nil, fmt.Errorf("fetch payloads from executor: %w", err)
	}

	var requestMap map[string]interface{}
	if err := json.Unmarshal(promptPayload, &requestMap); err != nil {
		return nil, fmt.Errorf("unmarshal prompt payload: %w", err)
	}

	originalResponse, err := completionapi.NewCompletionResponseFromLinesFromResponsePayload(responsePayload)
	if err != nil {
		return nil, fmt.Errorf("parse original response: %w", err)
	}

	enforcedTokens, err := originalResponse.GetEnforcedTokens()
	if err != nil {
		return nil, fmt.Errorf("get enforced tokens: %w", err)
	}

	requestMap["enforced_tokens"] = enforcedTokens
	requestMap["stream"] = false
	requestMap["skip_special_tokens"] = false
	delete(requestMap, "stream_options")

	validationBody, err := json.Marshal(requestMap)
	if err != nil {
		return nil, fmt.Errorf("marshal validation body: %w", err)
	}

	resp, err := broker.DoWithLockedNodeHTTPRetry(v.broker, req.Model, nil, 3,
		func(node *broker.Node) (*http.Response, *broker.ActionError) {
			url := node.InferenceUrlWithVersion(v.nodeVersion) + "/v1/chat/completions"
			httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(validationBody))
			if reqErr != nil {
				return nil, broker.NewApplicationActionError(reqErr)
			}
			httpReq.Header.Set("Content-Type", "application/json")
			httpResp, postErr := v.httpClient.Do(httpReq)
			if postErr != nil {
				return nil, broker.NewTransportActionError(postErr)
			}
			return httpResp, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("broker validate: %w", err)
	}
	defer resp.Body.Close()

	// 400/422 from ML node means enforced tokens not supported; treat as valid
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		return &subnet.ValidateResult{Valid: true}, nil
	}

	var respBytes []byte
	respBytes, err = readBody(resp)
	if err != nil {
		return nil, fmt.Errorf("read validation response: %w", err)
	}

	validationResponse, err := completionapi.NewCompletionResponseFromBytes(respBytes)
	if err != nil {
		return nil, fmt.Errorf("parse validation response: %w", err)
	}

	originalLogits := originalResponse.ExtractLogits()
	validationLogits := validationResponse.ExtractLogits()

	base := validation.BaseValidationResult{
		InferenceId:   inferenceID,
		ResponseBytes: respBytes,
	}

	result := validation.CompareLogits(originalLogits, validationLogits, base)

	return &subnet.ValidateResult{Valid: result.IsSuccessful()}, nil
}

func (v *ValidationAdapter) currentEpochID() uint64 {
	epochState := v.phaseTracker.GetCurrentEpochState()
	if epochState != nil {
		return epochState.LatestEpoch.EpochIndex
	}
	return 0
}

// fetchPayloadsFromExecutor retrieves payloads from the executor host using subnet session endpoint.
func (v *ValidationAdapter) fetchPayloadsFromExecutor(ctx context.Context, req subnet.ValidateRequest, inferenceID string, epochID uint64) ([]byte, []byte, error) {
	// Resolve executor URL from bridge
	executorInfo, err := v.bridge.GetValidatorInfo(req.ExecutorAddress)
	if err != nil {
		return nil, nil, fmt.Errorf("get executor info: %w", err)
	}
	if executorInfo.URL == "" {
		return nil, nil, fmt.Errorf("executor has no URL")
	}

	// Build request URL for subnet session endpoint
	requestURL, err := validation.BuildPayloadRequestURL(executorInfo.URL, fmt.Sprintf("subnet/v1/sessions/%s/payloads", req.EscrowID), inferenceID)
	if err != nil {
		return nil, nil, err
	}

	// Sign request
	timestamp := time.Now().UnixNano()
	validatorAddress := v.recorder.GetAccountAddress()
	signature, err := v.signPayloadRequest(inferenceID, timestamp, validatorAddress, epochID)
	if err != nil {
		return nil, nil, fmt.Errorf("sign request: %w", err)
	}

	// Fetch payloads using shared helper
	payloadResp, err := validation.FetchPayloadsHTTP(ctx, v.httpClient, requestURL, validatorAddress, timestamp, epochID, signature)
	if err != nil {
		return nil, nil, err
	}

	// Base64-encode raw pubkeys for signature verification
	encodedPubKeys := make([]string, len(req.ExecutorPubKeys))
	for i, pk := range req.ExecutorPubKeys {
		encodedPubKeys[i] = base64.StdEncoding.EncodeToString(pk)
	}

	// Verify executor signature
	if err := validation.VerifyExecutorPayloadSignature(
		inferenceID,
		payloadResp.PromptPayload,
		payloadResp.ResponsePayload,
		payloadResp.ExecutorSignature,
		req.ExecutorAddress,
		encodedPubKeys,
	); err != nil {
		return nil, nil, fmt.Errorf("verify executor signature: %w", err)
	}

	// Verify hashes against ValidateRequest
	expectedPromptHash := hex.EncodeToString(req.PromptHash)
	expectedResponseHash := hex.EncodeToString(req.ResponseHash)
	if err := validation.VerifyPayloadHashes(
		payloadResp.PromptPayload,
		payloadResp.ResponsePayload,
		expectedPromptHash,
		expectedResponseHash,
		inferenceID,
	); err != nil {
		return nil, nil, err
	}

	return payloadResp.PromptPayload, payloadResp.ResponsePayload, nil
}

// signPayloadRequest signs the payload retrieval request.
func (v *ValidationAdapter) signPayloadRequest(inferenceID string, timestamp int64, validatorAddress string, epochID uint64) (string, error) {
	components := calculations.SignatureComponents{
		Payload:         inferenceID,
		EpochId:         epochID,
		Timestamp:       timestamp,
		TransferAddress: validatorAddress,
		ExecutorAddress: "",
	}

	signerAddressStr := v.recorder.GetSignerAddress()
	signerAddress, err := sdk.AccAddressFromBech32(signerAddressStr)
	if err != nil {
		return "", err
	}
	accountSigner := &cmd.AccountSigner{
		Addr:    signerAddress,
		Keyring: v.recorder.GetKeyring(),
	}

	return calculations.Sign(accountSigner, components, calculations.Developer)
}

func readBody(resp *http.Response) ([]byte, error) {
	buf := new(bytes.Buffer)
	_, err := buf.ReadFrom(resp.Body)
	return buf.Bytes(), err
}

// Compile-time check.
var _ subnet.ValidationEngine = (*ValidationAdapter)(nil)
