package subnet

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"decentralized-api/cosmosclient"
	"decentralized-api/internal/validation"
	"decentralized-api/payloadstorage"
	"decentralized-api/utils"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/productscience/inference/cmd/inferenced/cmd"
	"github.com/productscience/inference/x/inference/calculations"
	inferenceTypes "github.com/productscience/inference/x/inference/types"

	subnetpkg "subnet"
	"subnet/bridge"
	"subnet/host"
	"subnet/signing"
	"subnet/state"
	"subnet/storage"
	"subnet/transport"
	"subnet/types"
)

type sessionEntry struct {
	server *transport.Server
	host   *host.Host
	store  storage.Storage
}

// HostManager manages per-escrow subnet sessions with lazy creation.
type HostManager struct {
	mu       sync.RWMutex
	sessions map[string]*sessionEntry

	signer       *signing.Secp256k1Signer
	verifier     signing.Verifier
	engine       subnetpkg.InferenceEngine
	validator    subnetpkg.ValidationEngine
	bridge       bridge.MainnetBridge
	payloadStore payloadstorage.PayloadStorage
	recorder     cosmosclient.CosmosMessageClient
}

func NewHostManager(
	signer *signing.Secp256k1Signer,
	engine subnetpkg.InferenceEngine,
	validator subnetpkg.ValidationEngine,
	br bridge.MainnetBridge,
	payloadStore payloadstorage.PayloadStorage,
	recorder cosmosclient.CosmosMessageClient,
) *HostManager {
	return &HostManager{
		sessions:     make(map[string]*sessionEntry),
		signer:       signer,
		verifier:     signing.NewSecp256k1Verifier(),
		engine:       engine,
		validator:    validator,
		bridge:       br,
		payloadStore: payloadStore,
		recorder:     recorder,
	}
}

func (m *HostManager) getOrCreate(escrowID string) (*sessionEntry, error) {
	m.mu.RLock()
	entry, ok := m.sessions[escrowID]
	m.mu.RUnlock()
	if ok {
		return entry, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok = m.sessions[escrowID]; ok {
		return entry, nil
	}
	return m.createLocked(escrowID)
}

func (m *HostManager) createLocked(escrowID string) (*sessionEntry, error) {
	group, err := bridge.BuildGroup(escrowID, m.bridge)
	if err != nil {
		return nil, fmt.Errorf("build group: %w", err)
	}

	escrow, err := m.bridge.GetEscrow(escrowID)
	if err != nil {
		return nil, fmt.Errorf("get escrow: %w", err)
	}

	creatorAddr := escrow.CreatorAddress

	config := types.DefaultSessionConfig(len(group))

	store := storage.NewMemory()
	if err := store.CreateSession(escrowID, config, group, escrow.Amount); err != nil {
		return nil, fmt.Errorf("init storage session: %w", err)
	}

	sm := state.NewStateMachine(escrowID, config, group, escrow.Amount, creatorAddr, m.verifier,
		state.WithWarmKeyResolver(m.bridge.VerifyWarmKey),
	)

	h, err := host.NewHost(sm, m.signer, m.engine, escrowID, group, nil,
		host.WithValidator(m.validator),
		host.WithStorage(store),
	)
	if err != nil {
		return nil, fmt.Errorf("create host: %w", err)
	}

	srv, err := transport.NewServer(h, store, escrowID, m.verifier, group, creatorAddr,
		transport.WithBridge(m.bridge),
	)
	if err != nil {
		return nil, fmt.Errorf("create server: %w", err)
	}

	entry := &sessionEntry{
		server: srv,
		host:   h,
		store:  store,
	}
	m.sessions[escrowID] = entry
	return entry, nil
}

// Register mounts subnet session routes on the given echo group.
func (m *HostManager) Register(g *echo.Group) {
	g.POST("/sessions/:id/chat/completions", m.withAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleInference }))
	g.POST("/sessions/:id/verify-timeout", m.withAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleVerifyTimeout }))
	g.POST("/sessions/:id/challenge-receipt", m.withAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleChallengeReceipt }))
	g.POST("/sessions/:id/gossip/nonce", m.withAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleGossipNonce }))
	g.POST("/sessions/:id/gossip/txs", m.withAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleGossipTxs }))
	g.GET("/sessions/:id/diffs", m.withoutAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleGetDiffs }))
	g.GET("/sessions/:id/mempool", m.withoutAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleGetMempool }))
	g.GET("/sessions/:id/signatures", m.withoutAuth(func(e *sessionEntry) echo.HandlerFunc { return e.server.HandleGetSignatures }))
	g.GET("/sessions/:id/payloads", m.handleGetPayloads)
}

// handleGetPayloads serves payloads to validators for subnet validation.
// Authenticates that the requester is a member of the session group (or a warm key
// for a group member), then returns signed payloads.
func (m *HostManager) handleGetPayloads(c echo.Context) error {
	escrowID := c.Param("id")
	inferenceID := c.QueryParam("inference_id")
	if inferenceID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "inference_id required"})
	}

	epochID, err := m.authenticatePayloadRequest(c, escrowID)
	if err != nil {
		return err
	}

	// Retrieve payloads with adjacent epoch fallback
	promptPayload, responsePayload, _, err := m.retrievePayloadsWithAdjacentEpochs(c.Request().Context(), escrowID, inferenceID, epochID)
	if err != nil {
		if errors.Is(err, payloadstorage.ErrNotFound) {
			return c.JSON(http.StatusNotFound, map[string]string{"error": "payload not found"})
		}
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	// Sign response using same scheme as public endpoint
	executorSignature, err := m.signPayloadResponse(inferenceID, promptPayload, responsePayload)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to sign response"})
	}

	return c.JSON(http.StatusOK, validation.PayloadResponse{
		InferenceId:       inferenceID,
		PromptPayload:     promptPayload,
		ResponsePayload:   responsePayload,
		ExecutorSignature: executorSignature,
	})
}

// authenticatePayloadRequest validates headers, timestamp, group membership,
// and signature for a payload retrieval request. Returns the parsed epochID.
func (m *HostManager) authenticatePayloadRequest(c echo.Context, escrowID string) (uint64, error) {
	validatorAddress := c.Request().Header.Get(utils.XValidatorAddressHeader)
	timestampStr := c.Request().Header.Get(utils.XTimestampHeader)
	epochIDStr := c.Request().Header.Get(utils.XEpochIdHeader)
	signature := c.Request().Header.Get(utils.AuthorizationHeader)
	inferenceID := c.QueryParam("inference_id")

	if validatorAddress == "" {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "X-Validator-Address header required"})
	}
	if timestampStr == "" {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "X-Timestamp header required"})
	}
	if epochIDStr == "" {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "X-Epoch-Id header required"})
	}
	if signature == "" {
		return 0, c.JSON(http.StatusUnauthorized, map[string]string{"error": "Authorization header required"})
	}

	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid timestamp format"})
	}

	epochID, err := strconv.ParseUint(epochIDStr, 10, 64)
	if err != nil {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "invalid epoch_id format"})
	}

	// Validate timestamp within 60s window
	now := time.Now().UnixNano()
	maxAge := int64(60 * time.Second)
	maxFuture := int64(10 * time.Second)
	requestAge := now - timestamp
	if requestAge > maxAge {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "request timestamp too old"})
	}
	if requestAge < -maxFuture {
		return 0, c.JSON(http.StatusBadRequest, map[string]string{"error": "request timestamp in the future"})
	}

	// Get session and verify group membership
	entry, err := m.getOrCreate(escrowID)
	if err != nil {
		return 0, c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	group := entry.host.Group()
	granterAddress, err := m.findGranterInGroup(validatorAddress, group)
	if err != nil {
		return 0, c.JSON(http.StatusUnauthorized, map[string]string{"error": "not a group member"})
	}

	// Collect requester's pubkeys for signature verification
	pubkeys, err := m.getValidatorPubKeys(c.Request().Context(), validatorAddress, granterAddress)
	if err != nil {
		return 0, c.JSON(http.StatusUnauthorized, map[string]string{"error": "failed to resolve validator pubkeys"})
	}

	// Verify signature
	components := calculations.SignatureComponents{
		Payload:         inferenceID,
		EpochId:         epochID,
		Timestamp:       timestamp,
		TransferAddress: validatorAddress,
		ExecutorAddress: "",
	}
	if err := calculations.ValidateSignatureWithGrantees(components, calculations.Developer, pubkeys, signature); err != nil {
		return 0, c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
	}

	return epochID, nil
}

// findGranterInGroup returns the group member address that the validator
// represents. If validatorAddress is a direct group member, returns it.
// Otherwise checks if validatorAddress is a warm key for any group member.
func (m *HostManager) findGranterInGroup(validatorAddress string, group []types.SlotAssignment) (string, error) {
	// Direct membership check
	for _, slot := range group {
		if slot.ValidatorAddress == validatorAddress {
			return validatorAddress, nil
		}
	}

	// Warm key check: see if validatorAddress is authorized by any group member
	for _, slot := range group {
		isWarm, err := m.bridge.VerifyWarmKey(validatorAddress, slot.ValidatorAddress)
		if err != nil {
			continue
		}
		if isWarm {
			return slot.ValidatorAddress, nil
		}
	}

	return "", fmt.Errorf("address %s is not a group member or warm key", validatorAddress)
}

// getValidatorPubKeys collects all pubkeys (cold + warm) that can sign on
// behalf of the validator. granterAddress is the group member address that
// the validator represents (may be the same as validatorAddress for direct members).
func (m *HostManager) getValidatorPubKeys(ctx context.Context, validatorAddress, granterAddress string) ([]string, error) {
	var pubkeys []string

	// Cold key from bridge
	info, err := m.bridge.GetValidatorInfo(granterAddress)
	if err == nil && len(info.PublicKey) > 0 {
		pubkeys = append(pubkeys, base64.StdEncoding.EncodeToString(info.PublicKey))
	}

	// Warm keys via grantees query
	queryClient := m.recorder.NewInferenceQueryClient()
	grantees, err := queryClient.GranteesByMessageType(ctx, &inferenceTypes.QueryGranteesByMessageTypeRequest{
		GranterAddress: granterAddress,
		MessageTypeUrl: "/inference.inference.MsgStartInference",
	})
	if err == nil {
		for _, g := range grantees.Grantees {
			pubkeys = append(pubkeys, g.PubKey)
		}
	}

	if len(pubkeys) == 0 {
		return nil, fmt.Errorf("no pubkeys found for %s (granter %s)", validatorAddress, granterAddress)
	}

	return pubkeys, nil
}

// retrievePayloadsWithAdjacentEpochs tries to retrieve payloads from storage,
// checking adjacent epochs if not found under the primary epochId.
func (m *HostManager) retrievePayloadsWithAdjacentEpochs(ctx context.Context, escrowID string, inferenceID string, epochID uint64) ([]byte, []byte, uint64, error) {
	// Use namespaced storage key to prevent cross-session collisions
	storageKey := fmt.Sprintf("subnet:%s:%s", escrowID, inferenceID)
	prompt, response, err := m.payloadStore.Retrieve(ctx, storageKey, epochID)
	if err == nil {
		return prompt, response, epochID, nil
	}
	if !errors.Is(err, payloadstorage.ErrNotFound) {
		return nil, nil, 0, err
	}

	// Try adjacent epochs (epoch boundary race condition)
	adjacentEpochs := []uint64{}
	if epochID > 0 {
		adjacentEpochs = append(adjacentEpochs, epochID-1)
	}
	adjacentEpochs = append(adjacentEpochs, epochID+1)

	for _, adjEpoch := range adjacentEpochs {
		prompt, response, err := m.payloadStore.Retrieve(ctx, storageKey, adjEpoch)
		if err == nil {
			return prompt, response, adjEpoch, nil
		}
		if !errors.Is(err, payloadstorage.ErrNotFound) {
			return nil, nil, 0, err
		}
	}

	return nil, nil, 0, payloadstorage.ErrNotFound
}

// signPayloadResponse signs the payload response using the same scheme as the public endpoint.
func (m *HostManager) signPayloadResponse(inferenceID string, promptPayload, responsePayload []byte) (string, error) {
	promptHash := utils.GenerateSHA256HashBytes(promptPayload)
	responseHash := utils.GenerateSHA256HashBytes(responsePayload)
	payload := inferenceID + promptHash + responseHash

	components := calculations.SignatureComponents{
		Payload:         payload,
		Timestamp:       0,
		TransferAddress: m.recorder.GetAccountAddress(),
		ExecutorAddress: "",
	}

	signerAddressStr := m.recorder.GetSignerAddress()
	signerAddress, err := sdk.AccAddressFromBech32(signerAddressStr)
	if err != nil {
		return "", err
	}
	accountSigner := &cmd.AccountSigner{
		Addr:    signerAddress,
		Keyring: m.recorder.GetKeyring(),
	}

	return calculations.Sign(accountSigner, components, calculations.Developer)
}

func (m *HostManager) withAuth(pick func(*sessionEntry) echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		entry, err := m.getOrCreate(c.Param("id"))
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return entry.server.AuthMiddleware(pick(entry))(c)
	}
}

func (m *HostManager) withoutAuth(pick func(*sessionEntry) echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		entry, err := m.getOrCreate(c.Param("id"))
		if err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return pick(entry)(c)
	}
}
