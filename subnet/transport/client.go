package transport

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	json "github.com/goccy/go-json"

	"subnet/host"
	"subnet/signing"
	"subnet/types"
)

// ClientConfig holds per-endpoint timeout settings.
type ClientConfig struct {
	InferenceTimeout time.Duration // /chat/completions, default 20m
	GossipTimeout    time.Duration // gossip/nonce, gossip/txs, default 10s
	VerifyTimeout    time.Duration // verify-timeout, default 3m
	QueryTimeout     time.Duration // diffs, mempool GETs, default 30s
}

func DefaultClientConfig() ClientConfig {
	return ClientConfig{
		InferenceTimeout: 20 * time.Minute,
		GossipTimeout:    10 * time.Second,
		VerifyTimeout:    3 * time.Minute,
		QueryTimeout:     30 * time.Second,
	}
}

// HTTPClient implements user.HostClient over HTTP.
type HTTPClient struct {
	baseURL  string
	escrowID string
	signer   signing.Signer
	http     *http.Client
	config   ClientConfig
}

// NewHTTPClient creates an HTTP client for the subnet transport layer.
// Uses shared transport for connection pooling, per-call context timeouts.
func NewHTTPClient(baseURL, escrowID string, signer signing.Signer, cfgs ...ClientConfig) *HTTPClient {
	cfg := DefaultClientConfig()
	if len(cfgs) > 0 {
		cfg = cfgs[0]
	}
	return &HTTPClient{
		baseURL:  baseURL,
		escrowID: escrowID,
		signer:   signer,
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     120 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		config: cfg,
	}
}

// Send implements user.HostClient.
func (c *HTTPClient) Send(ctx context.Context, req host.HostRequest) (*host.HostResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.InferenceTimeout)
	defer cancel()

	ir, err := HostRequestToJSON(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	body, err := json.Marshal(ir)
	if err != nil {
		return nil, fmt.Errorf("marshal json: %w", err)
	}

	respBody, err := c.doPost(ctx, "/sessions/"+c.escrowID+"/chat/completions", body)
	if err != nil {
		return nil, err
	}

	var respJSON InferenceResponse
	if err := json.Unmarshal(respBody, &respJSON); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	return HostResponseFromJSON(respJSON)
}

// GossipNonce sends a nonce notification to a peer.
func (c *HTTPClient) GossipNonce(ctx context.Context, nonce uint64, stateHash, stateSig []byte, slotID uint32) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.GossipTimeout)
	defer cancel()

	req := GossipNonceRequest{
		Nonce:     nonce,
		StateHash: stateHash,
		StateSig:  stateSig,
		SlotID:    slotID,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = c.doPost(ctx, "/sessions/"+c.escrowID+"/gossip/nonce", body)
	return err
}

// GossipTxs sends transactions to a peer.
func (c *HTTPClient) GossipTxs(ctx context.Context, txs []*types.SubnetTx) error {
	ctx, cancel := context.WithTimeout(ctx, c.config.GossipTimeout)
	defer cancel()

	txBytes, err := SubnetTxsToBytes(txs)
	if err != nil {
		return fmt.Errorf("encode txs: %w", err)
	}
	req := GossipTxsRequest{Txs: txBytes}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = c.doPost(ctx, "/sessions/"+c.escrowID+"/gossip/txs", body)
	return err
}

// SendVerifyTimeout asks a peer to verify a timeout (raw transport).
func (c *HTTPClient) SendVerifyTimeout(ctx context.Context, req VerifyTimeoutRequest) (*VerifyTimeoutResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.VerifyTimeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	respBody, err := c.doPost(ctx, "/sessions/"+c.escrowID+"/verify-timeout", body)
	if err != nil {
		return nil, err
	}
	var resp VerifyTimeoutResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &resp, nil
}

// ChallengeReceipt forwards diffs + payload to the executor and returns the receipt.
func (c *HTTPClient) ChallengeReceipt(ctx context.Context, inferenceID uint64, payload *host.InferencePayload, diffs []types.Diff) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.VerifyTimeout)
	defer cancel()

	djList := make([]DiffJSON, len(diffs))
	for i, d := range diffs {
		dj, err := DiffToJSON(d)
		if err != nil {
			return nil, fmt.Errorf("encode diff %d: %w", i, err)
		}
		djList[i] = dj
	}

	req := ChallengeReceiptRequest{
		InferenceID: inferenceID,
		Payload:     PayloadToJSON(payload),
		Diffs:       djList,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	respBody, err := c.doPost(ctx, "/sessions/"+c.escrowID+"/challenge-receipt", body)
	if err != nil {
		return nil, err
	}
	var resp ChallengeReceiptResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return resp.Receipt, nil
}

// VerifyTimeout implements user.TimeoutVerifier over HTTP.
func (c *HTTPClient) VerifyTimeout(ctx context.Context, inferenceID uint64, reason types.TimeoutReason, payload *host.InferencePayload) (bool, []byte, uint32, error) {
	resp, err := c.SendVerifyTimeout(ctx, VerifyTimeoutRequest{
		InferenceID: inferenceID,
		Reason:      TimeoutReasonToString(reason),
		Payload:     PayloadToJSON(payload),
	})
	if err != nil {
		return false, nil, 0, err
	}
	return resp.Accept, resp.Signature, resp.VoterSlot, nil
}

// GetDiffs fetches stored diffs from a peer.
func (c *HTTPClient) GetDiffs(ctx context.Context, from, to uint64) ([]types.Diff, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/subnet/v1/sessions/%s/diffs?from=%d&to=%d", c.baseURL, c.escrowID, from, to)
	respBody, err := c.doGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("get diffs: %w", err)
	}

	type diffRecordJSON struct {
		DiffJSON  `json:"diff"`
		StateHash []byte `json:"state_hash"`
	}
	var records []diffRecordJSON
	if err := json.Unmarshal(respBody, &records); err != nil {
		return nil, fmt.Errorf("unmarshal diffs: %w", err)
	}

	diffs := make([]types.Diff, len(records))
	for i, rec := range records {
		d, err := DiffFromJSON(rec.DiffJSON)
		if err != nil {
			return nil, fmt.Errorf("decode diff %d: %w", i, err)
		}
		diffs[i] = d
	}
	return diffs, nil
}

// GetSignatures fetches accumulated signatures for a nonce from a host.
func (c *HTTPClient) GetSignatures(ctx context.Context, nonce uint64) (map[uint32][]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/subnet/v1/sessions/%s/signatures?nonce=%d", c.baseURL, c.escrowID, nonce)
	respBody, err := c.doGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("get signatures: %w", err)
	}

	var resp SignaturesResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal signatures: %w", err)
	}
	return resp.Signatures, nil
}

// GetMempool fetches the host's current mempool.
func (c *HTTPClient) GetMempool(ctx context.Context) ([]*types.SubnetTx, error) {
	ctx, cancel := context.WithTimeout(ctx, c.config.QueryTimeout)
	defer cancel()

	url := fmt.Sprintf("%s/subnet/v1/sessions/%s/mempool", c.baseURL, c.escrowID)
	respBody, err := c.doGet(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("get mempool: %w", err)
	}

	var result struct {
		Txs [][]byte `json:"txs"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal mempool: %w", err)
	}
	return SubnetTxsFromBytes(result.Txs)
}

// doPost sends a signed POST request and returns the response body.
func (c *HTTPClient) doPost(ctx context.Context, path string, body []byte) ([]byte, error) {
	url := c.baseURL + "/subnet/v1" + path

	ts := time.Now().Unix()
	sig, err := SignRequest(c.signer, c.escrowID, body, ts)
	if err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, hex.EncodeToString(sig))
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http post %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %s: status %d: %s", path, resp.StatusCode, string(respBody))
	}

	return respBody, nil
}

// doGet sends a GET request and returns the response body.
// No auth signing -- GET endpoints skip auth on the server side for now.
func (c *HTTPClient) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %s: status %d", url, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}
