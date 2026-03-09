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

	"subnet/bridge"
	"subnet/gossip"
	"subnet/host"
	"subnet/logging"
	"subnet/signing"
	"subnet/storage"
	"subnet/types"
)

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
	bridge      bridge.MainnetBridge // optional, for warm key verification
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

// WithServerGossip attaches a gossip instance for nonce/tx propagation.
func WithServerGossip(g *gossip.Gossip) ServerOption {
	return func(s *Server) { s.gossip = g }
}

// WithServerPeerClients sets executor clients for timeout verification.
func WithServerPeerClients(peers map[int]*HTTPClient) ServerOption {
	return func(s *Server) { s.peerClients = peers }
}

// WithBridge sets the bridge for warm key verification in transport auth.
func WithBridge(b bridge.MainnetBridge) ServerOption {
	return func(s *Server) { s.bridge = b }
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
) (*Server, error) {
	if err := types.ValidateGroup(group); err != nil {
		return nil, err
	}
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
	return s, nil
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
	g.POST("/sessions/:id/challenge-receipt", s.handleChallengeReceipt)
	g.POST("/sessions/:id/gossip/nonce", s.handleGossipNonce)
	g.POST("/sessions/:id/gossip/txs", s.handleGossipTxs)
	// TODO: GET endpoints are intentionally unauthenticated for now.
	// Before production, restrict these to group members or add read-only auth.
	g.GET("/sessions/:id/diffs", s.handleGetDiffs)
	g.GET("/sessions/:id/mempool", s.handleGetMempool)
	g.GET("/sessions/:id/signatures", s.handleGetSignatures)
}

// writeJSON serializes v with goccy/go-json, bypassing Echo's default serializer.
func writeJSON(c echo.Context, code int, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.Blob(code, echo.MIMEApplicationJSON, b)
}

// errJSON writes a JSON error response.
func errJSON(c echo.Context, code int, msg string) error {
	return writeJSON(c, code, map[string]string{"error": msg})
}

// isAllowedSender returns true if addr is the session user, a group member,
// or a verified warm key for any group member.
func (s *Server) isAllowedSender(addr string) bool {
	if s.userAddr != "" && addr == s.userAddr {
		return true
	}
	for _, slot := range s.group {
		if slot.ValidatorAddress == addr {
			return true
		}
	}
	return s.isWarmKeySender(addr)
}

// isWarmKeySender checks if addr is a known warm key (from state) or can be
// verified via bridge for any group member. Cached by the bridge implementation.
func (s *Server) isWarmKeySender(addr string) bool {
	if s.host.IsWarmKeyAddress(addr) {
		return true
	}

	// Bridge fallback for gossip bootstrap.
	if s.bridge == nil {
		return false
	}
	seen := make(map[string]bool, len(s.group))
	for _, slot := range s.group {
		if seen[slot.ValidatorAddress] {
			continue
		}
		seen[slot.ValidatorAddress] = true
		ok, err := s.bridge.VerifyWarmKey(addr, slot.ValidatorAddress)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// isGroupMember returns true if addr is a group member (excludes the user).
// Gossip is host-to-host; the user has no business gossiping.
func (s *Server) isGroupMember(addr string) bool {
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
			return errJSON(c, http.StatusUnauthorized, "missing auth headers")
		}

		sig, err := hex.DecodeString(sigHex)
		if err != nil {
			return errJSON(c, http.StatusUnauthorized, "invalid signature hex")
		}

		ts, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			return errJSON(c, http.StatusUnauthorized, "invalid timestamp")
		}

		// Cap body size before reading.
		if s.maxBodySize > 0 {
			c.Request().Body = http.MaxBytesReader(c.Response(), c.Request().Body, s.maxBodySize)
		}

		body, err := io.ReadAll(c.Request().Body)
		if err != nil {
			return errJSON(c, http.StatusBadRequest, "read body")
		}

		now := time.Now().Unix()
		addr, err := VerifyRequest(s.verifier, s.escrowID, body, sig, ts, now)
		if err != nil {
			return errJSON(c, http.StatusUnauthorized, err.Error())
		}

		if !s.isAllowedSender(addr) {
			return errJSON(c, http.StatusForbidden, "sender not in group")
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
		return errJSON(c, http.StatusBadRequest, "invalid json: "+err.Error())
	}

	req, err := HostRequestFromJSON(ir)
	if err != nil {
		return errJSON(c, http.StatusBadRequest, "decode request: "+err.Error())
	}

	resp, err := s.host.HandleRequest(c.Request().Context(), req)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
	}

	respJSON, err := HostResponseToJSON(resp)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, "encode response: "+err.Error())
	}

	// Fire gossip in background if configured.
	if s.gossip != nil && resp.StateSig != nil {
		go s.gossip.AfterRequest(context.Background(), resp.Nonce, resp.StateHash, resp.StateSig)
	}

	// Lazy tx gossip: if signature was withheld (stale mempool) and mempool
	// is non-empty, broadcast txs so peers can include them.
	if s.gossip != nil && resp.StateSig == nil && len(resp.Mempool) > 0 {
		go s.gossip.BroadcastTxs(context.Background(), resp.Mempool)
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
		return errJSON(c, http.StatusBadRequest, "invalid json")
	}

	reason, err := TimeoutReasonFromString(req.Reason)
	if err != nil {
		return errJSON(c, http.StatusBadRequest, err.Error())
	}

	st := s.host.SnapshotState()
	localMempool := s.host.MempoolTxs()

	// Determine executor slot from inference_id.
	executorIdx := int(req.InferenceID % uint64(len(s.group)))
	var executorClient host.ExecutorClient
	if s.peerClients != nil {
		if pc, ok := s.peerClients[executorIdx]; ok {
			executorClient = pc
		}
	}

	nowUnix := time.Now().Unix()

	var accept bool
	switch reason {
	case types.TimeoutReason_TIMEOUT_REASON_REFUSED:
		// Fetch stored diffs to forward to executor during challenge.
		var storedDiffs []types.Diff
		if s.store != nil && st.LatestNonce > 0 {
			records, dErr := s.store.GetDiffs(s.escrowID, 1, st.LatestNonce)
			if dErr == nil {
				storedDiffs = make([]types.Diff, len(records))
				for i, r := range records {
					storedDiffs[i] = r.Diff
				}
			}
		}
		accept, err = host.VerifyRefusedTimeout(c.Request().Context(), st, req.InferenceID, PayloadFromJSON(req.Payload), storedDiffs, localMempool, executorClient, st.Config, nowUnix)
	case types.TimeoutReason_TIMEOUT_REASON_EXECUTION:
		accept, err = host.VerifyExecutionTimeout(c.Request().Context(), st, req.InferenceID, localMempool, executorClient, st.Config, nowUnix)
	default:
		return errJSON(c, http.StatusBadRequest, "unknown reason")
	}
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
	}

	resp := VerifyTimeoutResponse{Accept: accept}
	if accept {
		sig, voterSlot, sErr := signTimeoutVote(s.escrowID, req.InferenceID, reason, s.host.Signer(), s.host.SlotIDs())
		if sErr != nil {
			return errJSON(c, http.StatusInternalServerError, sErr.Error())
		}
		resp.Signature = sig
		resp.VoterSlot = voterSlot
	}
	return writeJSON(c, http.StatusOK, resp)
}

// signTimeoutVote marshals and signs a TimeoutVoteContent, returning the
// signature and the first slot ID from the host's owned slots.
func signTimeoutVote(escrowID string, inferenceID uint64, reason types.TimeoutReason, signer signing.Signer, slotIDs map[uint32]bool) ([]byte, uint32, error) {
	var voterSlot uint32
	for slot := range slotIDs {
		voterSlot = slot
		break
	}
	voteContent := &types.TimeoutVoteContent{
		EscrowId:    escrowID,
		InferenceId: inferenceID,
		Reason:      reason,
		Accept:      true,
	}
	voteData, err := proto.Marshal(voteContent)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal vote: %w", err)
	}
	sig, err := signer.Sign(voteData)
	if err != nil {
		return nil, 0, fmt.Errorf("sign vote: %w", err)
	}
	return sig, voterSlot, nil
}

func (s *Server) handleChallengeReceipt(c echo.Context) error {
	body := c.Get("body").([]byte)

	var req ChallengeReceiptRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid json")
	}

	diffs := make([]types.Diff, len(req.Diffs))
	for i, dj := range req.Diffs {
		d, err := DiffFromJSON(dj)
		if err != nil {
			return errJSON(c, http.StatusBadRequest, fmt.Sprintf("decode diff %d: %v", i, err))
		}
		diffs[i] = d
	}

	receipt, _, err := s.host.ChallengeReceipt(c.Request().Context(), req.InferenceID, PayloadFromJSON(req.Payload), diffs)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
	}

	return writeJSON(c, http.StatusOK, ChallengeReceiptResponse{Receipt: receipt})
}

func (s *Server) handleGossipNonce(c echo.Context) error {
	// Gossip is host-to-host only. Reject user-signed requests.
	sender := c.Get(contextKeySender).(string)
	if !s.isGroupMember(sender) {
		return errJSON(c, http.StatusForbidden, "gossip restricted to group members")
	}

	body := c.Get("body").([]byte)

	var req GossipNonceRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid json")
	}

	// Reject empty sig or invalid slot upfront. Without this, an attacker
	// can poison the seen map with a fake (nonce, hash) and cause false
	// equivocation detection against an honest host.
	if len(req.StateSig) == 0 {
		return errJSON(c, http.StatusBadRequest, "missing state signature")
	}
	if req.SlotID >= uint32(len(s.group)) {
		return errJSON(c, http.StatusBadRequest, "invalid slot id")
	}

	// Verify stateSig recovers to the claimed slot's address.
	// SlotIDs are compact 0..len(group)-1 so direct index is safe after bounds check above.
	expectedAddr := s.group[req.SlotID].ValidatorAddress

	sigContent := &types.StateSignatureContent{
		StateRoot: req.StateHash,
		EscrowId:  s.escrowID,
		Nonce:     req.Nonce,
	}
	sigData, err := proto.Marshal(sigContent)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, "marshal sig content")
	}
	addr, err := s.verifier.RecoverAddress(sigData, req.StateSig)
	if err != nil || addr != expectedAddr {
		return errJSON(c, http.StatusBadRequest, "invalid gossip state signature")
	}

	if s.gossip != nil {
		if err := s.gossip.OnNonceReceived(req.Nonce, req.StateHash, req.StateSig, req.SlotID); err != nil {
			return errJSON(c, http.StatusConflict, err.Error())
		}
	}

	// Accumulate sig directly if the host has this nonce backed.
	if err := s.host.AccumulateGossipSig(req.Nonce, req.StateHash, req.StateSig, req.SlotID); err != nil {
		logging.Debug("accumulate gossip sig skipped", "subsystem", "server", "nonce", req.Nonce, "error", err)
	}

	return c.NoContent(http.StatusOK)
}

func (s *Server) handleGossipTxs(c echo.Context) error {
	// Gossip is host-to-host only.
	sender := c.Get(contextKeySender).(string)
	if !s.isGroupMember(sender) {
		return errJSON(c, http.StatusForbidden, "gossip restricted to group members")
	}

	body := c.Get("body").([]byte)

	var req GossipTxsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid json")
	}

	if s.gossip != nil {
		txs, err := SubnetTxsFromBytes(req.Txs)
		if err != nil {
			return errJSON(c, http.StatusBadRequest, "decode txs: "+err.Error())
		}
		s.gossip.OnTxsReceived(txs)
	}

	return c.NoContent(http.StatusOK)
}

func (s *Server) handleGetSignatures(c echo.Context) error {
	nonceStr := c.QueryParam("nonce")
	if nonceStr == "" {
		return errJSON(c, http.StatusBadRequest, "missing 'nonce' parameter")
	}
	nonce, err := strconv.ParseUint(nonceStr, 10, 64)
	if err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid 'nonce' parameter")
	}

	sigs, err := s.host.GetSignatures(nonce)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
	}

	return writeJSON(c, http.StatusOK, SignaturesResponse{Signatures: sigs})
}

func (s *Server) handleGetDiffs(c echo.Context) error {
	if s.store == nil {
		return errJSON(c, http.StatusNotFound, "no storage configured")
	}

	fromStr := c.QueryParam("from")
	toStr := c.QueryParam("to")

	from, err := strconv.ParseUint(fromStr, 10, 64)
	if err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid 'from' parameter")
	}
	to, err := strconv.ParseUint(toStr, 10, 64)
	if err != nil {
		return errJSON(c, http.StatusBadRequest, "invalid 'to' parameter")
	}

	records, err := s.store.GetDiffs(s.escrowID, from, to)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
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
			return errJSON(c, http.StatusInternalServerError, fmt.Sprintf("encode diff %d: %v", rec.Nonce, err))
		}
		result[i] = diffRecordJSON{DiffJSON: dj, StateHash: rec.StateHash}
	}

	return writeJSON(c, http.StatusOK, result)
}

func (s *Server) handleGetMempool(c echo.Context) error {
	txs := s.host.MempoolTxs()
	data, err := SubnetTxsToBytes(txs)
	if err != nil {
		return errJSON(c, http.StatusInternalServerError, err.Error())
	}
	return writeJSON(c, http.StatusOK, map[string]interface{}{"txs": data})
}
