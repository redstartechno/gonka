package transport

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	json "github.com/goccy/go-json"
	"google.golang.org/protobuf/proto"

	"github.com/labstack/echo/v4"

	"subnet/gossip"
	"subnet/host"
	"subnet/signing"
	"subnet/storage"
	"subnet/types"
)

// contextKey is the echo context key for the recovered sender address.
const contextKeySender = "subnet_sender"

// Server wraps a host.Host and exposes it over HTTP via Echo.
type Server struct {
	host        *host.Host
	store       storage.Storage
	gossip      *gossip.Gossip // nil until gossip is wired
	escrowID    string
	verifier    signing.Verifier
	group       []types.SlotAssignment
	userAddr    string              // session user address, allowed alongside group members
	peerClients map[int]*HTTPClient // slot index -> client, for timeout verification
	rateLimit   *rateLimiter        // nil = no limiting
	maxBodySize int64               // max request body bytes, 0 = no limit
}

func protoMarshal(m proto.Message) ([]byte, error) { return proto.Marshal(m) }

// httpExecutorAdapter wraps HTTPClient to satisfy host.ExecutorClient.
type httpExecutorAdapter struct {
	client *HTTPClient
}

func (a *httpExecutorAdapter) GetMempool(ctx context.Context) ([]*types.SubnetTx, error) {
	return a.client.GetMempool(ctx)
}

func (a *httpExecutorAdapter) Send(ctx context.Context, req host.HostRequest) (*host.HostResponse, error) {
	return a.client.Send(ctx, req)
}

// ServerOption configures the Server.
type ServerOption func(*Server)

// WithRateLimit enables per-sender rate limiting.
func WithRateLimit(cfg RateLimitConfig) ServerOption {
	return func(s *Server) {
		s.rateLimit = newRateLimiter(cfg)
	}
}

// WithMaxBodySize sets the maximum request body size in bytes.
func WithMaxBodySize(n int64) ServerOption {
	return func(s *Server) {
		s.maxBodySize = n
	}
}

// NewServer creates an HTTP server wrapping the given host.
// userAddr is the session user's address -- allowed alongside group members.
func NewServer(
	h *host.Host,
	store storage.Storage,
	escrowID string,
	verifier signing.Verifier,
	group []types.SlotAssignment,
	userAddr string,
	opts ...ServerOption,
) *Server {
	s := &Server{
		host:     h,
		store:    store,
		escrowID: escrowID,
		verifier: verifier,
		group:    group,
		userAddr: userAddr,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// SetGossip attaches a gossip instance for nonce/tx propagation.
func (s *Server) SetGossip(g *gossip.Gossip) { s.gossip = g }

// Register mounts all subnet routes on the given echo group.
// The caller typically mounts this under /subnet/v1.
func (s *Server) Register(g *echo.Group) {
	g.Use(s.authMiddleware)
	if s.rateLimit != nil {
		g.Use(rateLimitMiddleware(s.rateLimit))
	}
	g.POST("/sessions/:id/chat/completions", s.handleInference)
	g.POST("/sessions/:id/verify-timeout", s.handleVerifyTimeout)
	g.POST("/sessions/:id/gossip/nonce", s.handleGossipNonce)
	g.POST("/sessions/:id/gossip/txs", s.handleGossipTxs)
	// GET endpoints intentionally skip group membership check for now.
	g.GET("/sessions/:id/diffs", s.handleGetDiffs)
	g.GET("/sessions/:id/mempool", s.handleGetMempool)
}

// writeJSON serializes v with goccy/go-json, bypassing Echo's default serializer.
func writeJSON(c echo.Context, code int, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Blob(code, echo.MIMEApplicationJSON, b)
}

// isAllowedSender returns true if addr is the session user or a group member.
func (s *Server) isAllowedSender(addr string) bool {
	if s.userAddr != "" && addr == s.userAddr {
		return true
	}
	for _, slot := range s.group {
		if slot.ValidatorAddress == addr {
			return true
		}
	}
	return false
}

// authMiddleware reads the body, verifies the signature, checks group membership,
// and stores the sender address in the echo context.
// GET requests skip auth intentionally for now.
func (s *Server) authMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		if c.Request().Method == http.MethodGet {
			// GET endpoints skip auth for now -- see Register comment.
			return next(c)
		}

		sigHex := c.Request().Header.Get(HeaderSignature)
		tsStr := c.Request().Header.Get(HeaderTimestamp)
		if sigHex == "" || tsStr == "" {
			return writeJSON(c, http.StatusUnauthorized, map[string]string{"error": "missing auth headers"})
		}

		sig, err := hex.DecodeString(sigHex)
		if err != nil {
			return writeJSON(c, http.StatusUnauthorized, map[string]string{"error": "invalid signature hex"})
		}

		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			return writeJSON(c, http.StatusUnauthorized, map[string]string{"error": "invalid timestamp"})
		}

		// Cap body size before reading.
		if s.maxBodySize > 0 {
			c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, s.maxBodySize)
		}

		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "read body"})
		}

		now := time.Now().Unix()
		addr, err := VerifyRequest(s.verifier, s.escrowID, body, sig, ts, now)
		if err != nil {
			return writeJSON(c, http.StatusUnauthorized, map[string]string{"error": err.Error()})
		}

		if !s.isAllowedSender(addr) {
			return writeJSON(c, http.StatusForbidden, map[string]string{"error": "sender not in group"})
		}

		// Store sender and re-inject body for handler.
		c.Set(contextKeySender, addr)
		c.Set("body", body)
		return next(c)
	}
}

func (s *Server) handleInference(c echo.Context) error {
	body := c.Get("body").([]byte)

	var ir InferenceRequest
	if err := json.Unmarshal(body, &ir); err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid json: " + err.Error()})
	}

	req, err := HostRequestFromJSON(ir)
	if err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "decode request: " + err.Error()})
	}

	resp, err := s.host.HandleRequest(c.Request().Context(), req)
	if err != nil {
		return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	respJSON, err := HostResponseToJSON(resp)
	if err != nil {
		return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": "encode response: " + err.Error()})
	}

	// Fire gossip in background if configured.
	if s.gossip != nil && resp.StateSig != nil {
		go s.gossip.AfterRequest(context.Background(), resp.Nonce, resp.StateHash, resp.StateSig)
	}

	return writeJSON(c, http.StatusOK, respJSON)
}

// SetPeerClients sets the executor clients for timeout verification.
// Key is slot index (position in group), value is an ExecutorClient.
func (s *Server) SetPeerClients(peers map[int]*HTTPClient) {
	s.peerClients = peers
}

func (s *Server) handleVerifyTimeout(c echo.Context) error {
	body := c.Get("body").([]byte)

	var req VerifyTimeoutRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid json"})
	}

	reason, err := TimeoutReasonFromString(req.Reason)
	if err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": err.Error()})
	}

	st := s.host.SnapshotState()
	localMempool := s.host.MempoolTxs()

	// Determine executor slot from inference_id.
	executorIdx := int(req.InferenceID % uint64(len(s.group)))
	var executorClient host.ExecutorClient
	if s.peerClients != nil {
		if pc, ok := s.peerClients[executorIdx]; ok {
			executorClient = &httpExecutorAdapter{client: pc}
		}
	}

	var accept bool
	switch reason {
	case types.TimeoutReason_TIMEOUT_REASON_REFUSED:
		accept, err = host.VerifyRefusedTimeout(c.Request().Context(), st, req.InferenceID, req.PromptData, localMempool, executorClient, st.Config)
	case types.TimeoutReason_TIMEOUT_REASON_EXECUTION:
		accept, err = host.VerifyExecutionTimeout(c.Request().Context(), st, req.InferenceID, localMempool, executorClient, st.Config)
	default:
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "unknown reason"})
	}
	if err != nil {
		return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	resp := VerifyTimeoutResponse{Accept: accept}

	// If accepted, sign a timeout vote.
	if accept {
		// Pick the first slot this host owns.
		var voterSlot uint32
		for slot := range s.host.SlotIDs() {
			voterSlot = slot
			break
		}

		voteContent := &types.TimeoutVoteContent{
			EscrowId:    s.escrowID,
			InferenceId: req.InferenceID,
			Reason:      reason,
			Accept:      true,
		}
		voteData, err := protoMarshal(voteContent)
		if err != nil {
			return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": "marshal vote"})
		}
		sig, err := s.host.Signer().Sign(voteData)
		if err != nil {
			return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": "sign vote"})
		}
		resp.Signature = sig
		resp.VoterSlot = voterSlot
	}

	return writeJSON(c, http.StatusOK, resp)
}

func (s *Server) handleGossipNonce(c echo.Context) error {
	body := c.Get("body").([]byte)

	var req GossipNonceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid json"})
	}

	if s.gossip != nil {
		if err := s.gossip.OnNonceReceived(req.Nonce, req.StateHash, req.StateSig, req.SlotID); err != nil {
			return writeJSON(c, http.StatusConflict, map[string]string{"error": err.Error()})
		}
	}

	return c.NoContent(http.StatusOK)
}

func (s *Server) handleGossipTxs(c echo.Context) error {
	body := c.Get("body").([]byte)

	var req GossipTxsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid json"})
	}

	if s.gossip != nil {
		txs, err := SubnetTxsFromBytes(req.Txs)
		if err != nil {
			return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "decode txs: " + err.Error()})
		}
		s.gossip.OnTxsReceived(txs)
	}

	return c.NoContent(http.StatusOK)
}

func (s *Server) handleGetDiffs(c echo.Context) error {
	if s.store == nil {
		return writeJSON(c, http.StatusNotFound, map[string]string{"error": "no storage configured"})
	}

	fromStr := c.QueryParam("from")
	toStr := c.QueryParam("to")

	from, err := strconv.ParseUint(fromStr, 10, 64)
	if err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid 'from' parameter"})
	}
	to, err := strconv.ParseUint(toStr, 10, 64)
	if err != nil {
		return writeJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid 'to' parameter"})
	}

	records, err := s.store.GetDiffs(s.escrowID, from, to)
	if err != nil {
		return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Convert to JSON-friendly format.
	type diffRecordJSON struct {
		DiffJSON  `json:"diff"`
		StateHash []byte `json:"state_hash"`
	}

	result := make([]diffRecordJSON, len(records))
	for i, rec := range records {
		dj, err := DiffToJSON(rec.Diff)
		if err != nil {
			return writeJSON(c, http.StatusInternalServerError, map[string]string{
				"error": fmt.Sprintf("encode diff %d: %v", rec.Nonce, err),
			})
		}
		result[i] = diffRecordJSON{DiffJSON: dj, StateHash: rec.StateHash}
	}

	return writeJSON(c, http.StatusOK, result)
}

func (s *Server) handleGetMempool(c echo.Context) error {
	txs := s.host.MempoolTxs()
	data, err := SubnetTxsToBytes(txs)
	if err != nil {
		return writeJSON(c, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
	return writeJSON(c, http.StatusOK, map[string]interface{}{"txs": data})
}
