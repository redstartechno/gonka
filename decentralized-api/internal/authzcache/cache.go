package authzcache

import (
	"context"
	"decentralized-api/cosmosclient"
	"decentralized-api/logging"
	"sync"
	"time"

	"github.com/productscience/inference/x/inference/types"
)

const authzCacheTTL = 2 * time.Minute

type cachedEntry struct {
	pubkeys   []string
	expiresAt time.Time
}

// AuthzCache caches public keys for granter addresses to avoid repeated chain queries.
// Keys are cached with TTL since authz grants can change.
type AuthzCache struct {
	mu       sync.RWMutex
	cache    map[string]*cachedEntry // granterAddress -> entry
	recorder cosmosclient.CosmosMessageClient
}

func NewAuthzCache(recorder cosmosclient.CosmosMessageClient) *AuthzCache {
	return &AuthzCache{
		cache:    make(map[string]*cachedEntry),
		recorder: recorder,
	}
}

// GetPubKeys returns all public keys authorized to sign on behalf of granterAddress.
// Includes granter's own key plus any grantee keys via authz.
// Results are cached with TTL.
func (c *AuthzCache) GetPubKeys(ctx context.Context, granterAddress, msgTypeUrl string) ([]string, error) {
	cacheKey := granterAddress + "|" + msgTypeUrl

	c.mu.RLock()
	if entry, ok := c.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		pubkeys := entry.pubkeys
		c.mu.RUnlock()
		return pubkeys, nil
	}
	c.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Double-check after acquiring write lock
	if entry, ok := c.cache[cacheKey]; ok && time.Now().Before(entry.expiresAt) {
		return entry.pubkeys, nil
	}

	logging.Debug("Fetching authz pubkeys", types.Validation,
		"granterAddress", granterAddress, "msgTypeUrl", msgTypeUrl)

	queryClient := c.recorder.NewInferenceQueryClient()

	// Get grantees (warm keys) for this message type
	grantees, err := queryClient.GranteesByMessageType(ctx, &types.QueryGranteesByMessageTypeRequest{
		GranterAddress: granterAddress,
		MessageTypeUrl: msgTypeUrl,
	})
	if err != nil {
		return nil, err
	}

	// Get granter's own public key
	participant, err := queryClient.InferenceParticipant(ctx, &types.QueryInferenceParticipantRequest{
		Address: granterAddress,
	})
	if err != nil {
		return nil, err
	}

	// Collect all pubkeys: grantees + granter
	pubkeys := make([]string, 0, len(grantees.Grantees)+1)
	for _, grantee := range grantees.Grantees {
		pubkeys = append(pubkeys, grantee.PubKey)
	}
	pubkeys = append(pubkeys, participant.Pubkey)

	c.cache[cacheKey] = &cachedEntry{
		pubkeys:   pubkeys,
		expiresAt: time.Now().Add(authzCacheTTL),
	}

	logging.Debug("Cached authz pubkeys", types.Validation,
		"granterAddress", granterAddress, "count", len(pubkeys))

	return pubkeys, nil
}
