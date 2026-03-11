package transport

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	json "github.com/goccy/go-json"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"subnet/host"
	"subnet/internal/testutil"
	"subnet/signing"
	"subnet/state"
	"subnet/storage"
	"subnet/stub"
	"subnet/types"
)

type serverTestEnv struct {
	server     *Server
	echo       *echo.Echo
	store      *storage.Memory
	userSigner *signing.Secp256k1Signer
	hostSigner *signing.Secp256k1Signer
	group      []types.SlotAssignment
	config     types.SessionConfig
}

func setupServerEnv(t *testing.T) *serverTestEnv {
	t.Helper()
	hostSigner := testutil.MustGenerateKey(t)
	userSigner := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup([]*signing.Secp256k1Signer{hostSigner})
	config := testutil.DefaultConfig(1)
	verifier := signing.NewSecp256k1Verifier()

	sm := state.NewStateMachine("escrow-1", config, group, 100000, userSigner.Address(), verifier)
	engine := stub.NewInferenceEngine()
	store := storage.NewMemory()
	require.NoError(t, store.CreateSession(storage.CreateSessionParams{EscrowID: "escrow-1", Config: config, Group: group, InitialBalance: 100000}))

	h, err := host.NewHost(sm, hostSigner, engine, "escrow-1", group, nil, host.WithGrace(100), host.WithStorage(store))
	require.NoError(t, err)

	srv, err := NewServer(h, store, "escrow-1", verifier, group, userSigner.Address())
	require.NoError(t, err)

	e := echo.New()
	g := e.Group("/subnet/v1")
	srv.Register(g)

	return &serverTestEnv{
		server:     srv,
		echo:       e,
		store:      store,
		userSigner: userSigner,
		hostSigner: hostSigner,
		group:      group,
		config:     config,
	}
}

func (env *serverTestEnv) doPost(t *testing.T, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	ts := time.Now().Unix()
	sig, err := SignRequest(env.userSigner, "escrow-1", body, ts)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, hex.EncodeToString(sig))
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", ts))
	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)
	return rec
}

func (env *serverTestEnv) doGet(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)
	return rec
}

func TestServer_Inference_ValidAuth(t *testing.T) {
	env := setupServerEnv(t)

	// Build a valid inference request.
	diff := testutil.SignDiff(t, env.userSigner, "escrow-1", 1, []*types.SubnetTx{testutil.StartTx(1)})
	dj, err := DiffToJSON(diff)
	require.NoError(t, err)

	ir := InferenceRequest{
		Diffs: []DiffJSON{dj},
		Nonce: 1,
		Payload: &PayloadJSON{
			Prompt:      testutil.TestPrompt,
			Model:       "llama",
			InputLength: 100,
			MaxTokens:   50,
			StartedAt:   1000,
		},
	}
	body, err := json.Marshal(ir)
	require.NoError(t, err)

	rec := env.doPost(t, "/subnet/v1/sessions/escrow-1/chat/completions", body)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp InferenceResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, uint64(1), resp.Nonce)
	require.NotNil(t, resp.StateSig)
	require.NotNil(t, resp.Receipt) // single host is always executor
	require.NotEmpty(t, resp.Mempool)
}

func TestServer_Inference_NoAuth(t *testing.T) {
	env := setupServerEnv(t)

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/subnet/v1/sessions/escrow-1/chat/completions",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestServer_Inference_NotInGroup(t *testing.T) {
	env := setupServerEnv(t)

	outsider := testutil.MustGenerateKey(t)
	body := []byte(`{}`)
	ts := time.Now().Unix()
	sig, err := SignRequest(outsider, "escrow-1", body, ts)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/subnet/v1/sessions/escrow-1/chat/completions",
		strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, hex.EncodeToString(sig))
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", ts))
	rec := httptest.NewRecorder()
	env.echo.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestServer_GetDiffs(t *testing.T) {
	env := setupServerEnv(t)

	// First apply a diff via the inference endpoint.
	diff := testutil.SignDiff(t, env.userSigner, "escrow-1", 1, []*types.SubnetTx{testutil.StartTx(1)})
	dj, err := DiffToJSON(diff)
	require.NoError(t, err)
	ir := InferenceRequest{
		Diffs:   []DiffJSON{dj},
		Nonce:   1,
		Payload: &PayloadJSON{Prompt: testutil.TestPrompt, Model: "llama", InputLength: 100, MaxTokens: 50, StartedAt: 1000},
	}
	body, _ := json.Marshal(ir)
	rec := env.doPost(t, "/subnet/v1/sessions/escrow-1/chat/completions", body)
	require.Equal(t, http.StatusOK, rec.Code)

	// Now GET diffs.
	rec = env.doGet(t, "/subnet/v1/sessions/escrow-1/diffs?from=1&to=1")
	require.Equal(t, http.StatusOK, rec.Code)

	var diffs []json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &diffs))
	require.Len(t, diffs, 1)
}

func TestServer_GetMempool(t *testing.T) {
	env := setupServerEnv(t)

	// Apply a diff to populate the mempool with MsgFinishInference.
	diff := testutil.SignDiff(t, env.userSigner, "escrow-1", 1, []*types.SubnetTx{testutil.StartTx(1)})
	dj, err := DiffToJSON(diff)
	require.NoError(t, err)
	ir := InferenceRequest{
		Diffs:   []DiffJSON{dj},
		Nonce:   1,
		Payload: &PayloadJSON{Prompt: testutil.TestPrompt, Model: "llama", InputLength: 100, MaxTokens: 50, StartedAt: 1000},
	}
	body, _ := json.Marshal(ir)
	rec := env.doPost(t, "/subnet/v1/sessions/escrow-1/chat/completions", body)
	require.Equal(t, http.StatusOK, rec.Code)

	// GET mempool.
	rec = env.doGet(t, "/subnet/v1/sessions/escrow-1/mempool")
	require.Equal(t, http.StatusOK, rec.Code)

	var result struct {
		Txs [][]byte `json:"txs"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
	require.NotEmpty(t, result.Txs)
}

func TestServer_RateLimit(t *testing.T) {
	env := setupServerEnv(t)

	// Re-create server with a tight rate limit.
	srv, err := NewServer(env.server.host, env.store, "escrow-1",
		env.server.verifier, env.group, env.userSigner.Address(),
		WithRateLimit(RateLimitConfig{RequestsPerSecond: 1, BurstSize: 1}))
	require.NoError(t, err)

	e := echo.New()
	g := e.Group("/subnet/v1")
	srv.Register(g)

	body := []byte(`{}`)
	doReq := func() int {
		ts := time.Now().Unix()
		sig, _ := SignRequest(env.userSigner, "escrow-1", body, ts)
		req := httptest.NewRequest(http.MethodPost, "/subnet/v1/sessions/escrow-1/chat/completions",
			strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderSignature, hex.EncodeToString(sig))
		req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", ts))
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		return rec.Code
	}

	// First request should pass (burst=1).
	code := doReq()
	// Could be 200 or 400 (bad inference body), but not 429.
	require.NotEqual(t, http.StatusTooManyRequests, code)

	// Second request should be rate limited.
	code = doReq()
	require.Equal(t, http.StatusTooManyRequests, code)
}

func TestHandleGossipNonce_WarmKey(t *testing.T) {
	// Set up: host signer at slot 0, warm key for slot 0.
	hostSigner := testutil.MustGenerateKey(t)
	warmSigner := testutil.MustGenerateKey(t)
	userSigner := testutil.MustGenerateKey(t)
	group := testutil.MakeGroup([]*signing.Secp256k1Signer{hostSigner})
	config := testutil.DefaultConfig(1)
	verifier := signing.NewSecp256k1Verifier()

	resolver := func(warmAddr, coldAddr string) (bool, error) {
		return warmAddr == warmSigner.Address() && coldAddr == hostSigner.Address(), nil
	}

	sm := state.NewStateMachine("escrow-1", config, group, 100000, userSigner.Address(), verifier, state.WithWarmKeyResolver(resolver))

	// Create warm key binding via confirm start.
	diff1 := testutil.SignDiff(t, userSigner, "escrow-1", 1, []*types.SubnetTx{testutil.StartTx(1)})
	_, err := sm.ApplyDiff(diff1)
	require.NoError(t, err)

	// inference 1 % 1 = 0, executor = slot 0.
	execSig := testutil.SignExecutorReceipt(t, warmSigner, "escrow-1", 1, testutil.TestPromptHash[:], "llama", 100, 50, 1000, 1000)
	confirmTx := &types.SubnetTx{Tx: &types.SubnetTx_ConfirmStart{ConfirmStart: &types.MsgConfirmStart{
		InferenceId: 1, ExecutorSig: execSig, ConfirmedAt: 1000,
	}}}
	diff2 := testutil.SignDiff(t, userSigner, "escrow-1", 2, []*types.SubnetTx{confirmTx})
	_, err = sm.ApplyDiff(diff2)
	require.NoError(t, err)

	store := storage.NewMemory()
	require.NoError(t, store.CreateSession(storage.CreateSessionParams{EscrowID: "escrow-1", Config: config, Group: group, InitialBalance: 100000}))

	// Rebuild SM from scratch for host (host needs nonce 0 start).
	sm2 := state.NewStateMachine("escrow-1", config, group, 100000, userSigner.Address(), verifier, state.WithWarmKeyResolver(resolver))
	engine := stub.NewInferenceEngine()
	h, err := host.NewHost(sm2, hostSigner, engine, "escrow-1", group, nil, host.WithGrace(100), host.WithStorage(store), host.WithVerifier(verifier))
	require.NoError(t, err)

	srv, err := NewServer(h, store, "escrow-1", verifier, group, userSigner.Address())
	require.NoError(t, err)

	e := echo.New()
	g := e.Group("/subnet/v1")
	srv.Register(g)

	// Apply diffs through the host to populate storage.
	_, err = h.HandleRequest(context.Background(), host.HostRequest{Diffs: []types.Diff{diff1, diff2}})
	require.NoError(t, err)

	// Compute state root for signing.
	stateRoot, err := h.StateRoot()
	require.NoError(t, err)

	// Sign state with warm key.
	sigContent := &types.StateSignatureContent{
		StateRoot: stateRoot,
		EscrowId:  "escrow-1",
		Nonce:     2,
	}
	sigData, merr := proto.Marshal(sigContent)
	require.NoError(t, merr)
	warmStateSig, err := warmSigner.Sign(sigData)
	require.NoError(t, err)

	// Build gossip nonce request.
	nonceReq := GossipNonceRequest{
		Nonce:     2,
		StateHash: stateRoot,
		StateSig:  warmStateSig,
		SlotID:    0,
	}
	body, err := json.Marshal(nonceReq)
	require.NoError(t, err)

	// Sign the HTTP request with warm key (warm key is a group member via bridge).
	ts := time.Now().Unix()
	sig, err := SignRequest(warmSigner, "escrow-1", body, ts)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/subnet/v1/sessions/escrow-1/gossip/nonce", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderSignature, hex.EncodeToString(sig))
	req.Header.Set(HeaderTimestamp, fmt.Sprintf("%d", ts))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "warm key gossip nonce should succeed, got: %s", rec.Body.String())
}
