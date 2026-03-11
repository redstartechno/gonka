package user

import (
	"fmt"

	"subnet/bridge"
	"subnet/signing"
	"subnet/state"
	"subnet/storage"
	"subnet/transport"
	"subnet/types"
)

// HTTPSessionConfig holds the parameters needed to create an HTTP-backed user session.
type HTTPSessionConfig struct {
	PrivateKeyHex string
	EscrowID      string
	Bridge        bridge.MainnetBridge
	StoragePath   string // optional: path to SQLite DB for session persistence
}

// NewHTTPSession creates a user Session wired with HTTP clients to real dapi hosts.
// It queries the bridge for escrow and group info, then creates transport clients
// for each slot.
func NewHTTPSession(cfg HTTPSessionConfig) (*Session, *state.StateMachine, error) {
	signer, err := signing.SignerFromHex(cfg.PrivateKeyHex)
	if err != nil {
		return nil, nil, fmt.Errorf("create signer: %w", err)
	}
	verifier := signing.NewSecp256k1Verifier()

	group, err := bridge.BuildGroup(cfg.EscrowID, cfg.Bridge)
	if err != nil {
		return nil, nil, fmt.Errorf("build group: %w", err)
	}

	escrow, err := cfg.Bridge.GetEscrow(cfg.EscrowID)
	if err != nil {
		return nil, nil, fmt.Errorf("get escrow: %w", err)
	}

	config := types.DefaultSessionConfig(len(group))

	sm := state.NewStateMachine(cfg.EscrowID, config, group, escrow.Amount, escrow.CreatorAddress, verifier,
		state.WithWarmKeyResolver(cfg.Bridge.VerifyWarmKey),
	)

	clients := make([]HostClient, len(group))
	clientCache := make(map[string]*transport.HTTPClient)
	for i, slot := range group {
		if c, ok := clientCache[slot.ValidatorAddress]; ok {
			clients[i] = c
			continue
		}
		info, err := cfg.Bridge.GetHostInfo(slot.ValidatorAddress)
		if err != nil {
			return nil, nil, fmt.Errorf("get host info for %s: %w", slot.ValidatorAddress, err)
		}
		c := transport.NewHTTPClient(info.URL, cfg.EscrowID, signer)
		clientCache[slot.ValidatorAddress] = c
		clients[i] = c
	}

	var opts []SessionOption
	if cfg.StoragePath != "" {
		sqlStore, storeErr := storage.NewSQLite(cfg.StoragePath)
		if storeErr != nil {
			return nil, nil, fmt.Errorf("open storage: %w", storeErr)
		}
		opts = append(opts, WithStorage(sqlStore))

		// Check if there are existing diffs to recover from.
		meta, metaErr := sqlStore.GetSessionMeta(cfg.EscrowID)
		if metaErr == nil && meta.LatestNonce > 0 {
			session, recSM, recErr := RecoverSession(sqlStore, signer, verifier, cfg.EscrowID, group, clients)
			if recErr != nil {
				sqlStore.Close()
				return nil, nil, fmt.Errorf("recover session: %w", recErr)
			}
			return session, recSM, nil
		}

		// First run: create the session row so AppendDiff works later.
		if createErr := sqlStore.CreateSession(storage.CreateSessionParams{
			EscrowID:       cfg.EscrowID,
			CreatorAddr:    escrow.CreatorAddress,
			Config:         config,
			Group:          group,
			InitialBalance: escrow.Amount,
		}); createErr != nil {
			sqlStore.Close()
			return nil, nil, fmt.Errorf("create storage session: %w", createErr)
		}
	}

	session, err := NewSession(sm, signer, cfg.EscrowID, group, clients, verifier, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create session: %w", err)
	}

	return session, sm, nil
}
