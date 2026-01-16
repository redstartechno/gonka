package pocartifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"decentralized-api/logging"

	"github.com/productscience/inference/x/inference/types"
)

// ManagedArtifactStore wraps per-epoch ArtifactStores with automatic pruning.
// - Creates separate directories for each poc_stage_start_block_height
// - Automatically prunes old stores in background, keeping the newest N
// - Aligned with payloadstorage/ManagedStorage pattern
type ManagedArtifactStore struct {
	mu          sync.RWMutex
	baseDir     string
	stores      map[int64]*ArtifactStore // poc_stage_start_block_height -> store
	retainCount int                      // keep newest N stores
	cancel      context.CancelFunc       // cancels cleanup goroutine
}

// NewManagedArtifactStore creates a new managed store with automatic pruning.
// retainCount specifies how many recent stores to keep (based on block height).
func NewManagedArtifactStore(baseDir string, retainCount int) *ManagedArtifactStore {
	ctx, cancel := context.WithCancel(context.Background())
	m := &ManagedArtifactStore{
		baseDir:     baseDir,
		stores:      make(map[int64]*ArtifactStore),
		retainCount: retainCount,
		cancel:      cancel,
	}
	go m.cleanupLoop(ctx)
	return m
}

// GetOrCreateStore returns the store for the given block height, creating it if needed.
func (m *ManagedArtifactStore) GetOrCreateStore(pocStageStartHeight int64) (*ArtifactStore, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if store, ok := m.stores[pocStageStartHeight]; ok {
		return store, nil
	}

	storeDir := filepath.Join(m.baseDir, strconv.FormatInt(pocStageStartHeight, 10))
	store, err := Open(storeDir)
	if err != nil {
		return nil, fmt.Errorf("open store for height %d: %w", pocStageStartHeight, err)
	}

	m.stores[pocStageStartHeight] = store
	return store, nil
}

// GetStore returns the store for the given block height, or an error if it doesn't exist.
// Does not create new stores (for proof requests).
func (m *ManagedArtifactStore) GetStore(pocStageStartHeight int64) (*ArtifactStore, error) {
	m.mu.RLock()
	store, ok := m.stores[pocStageStartHeight]
	m.mu.RUnlock()

	if ok {
		return store, nil
	}

	// Try to open from disk (may exist from previous run)
	storeDir := filepath.Join(m.baseDir, strconv.FormatInt(pocStageStartHeight, 10))
	if _, err := os.Stat(storeDir); os.IsNotExist(err) {
		return nil, fmt.Errorf("store for height %d not found", pocStageStartHeight)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Double-check after acquiring write lock
	if store, ok := m.stores[pocStageStartHeight]; ok {
		return store, nil
	}

	store, err := Open(storeDir)
	if err != nil {
		return nil, fmt.Errorf("open store for height %d: %w", pocStageStartHeight, err)
	}

	m.stores[pocStageStartHeight] = store
	return store, nil
}

// PruneStore removes the store directory and closes any open store.
func (m *ManagedArtifactStore) PruneStore(pocStageStartHeight int64) error {
	m.mu.Lock()
	if store, ok := m.stores[pocStageStartHeight]; ok {
		store.Close()
		delete(m.stores, pocStageStartHeight)
	}
	m.mu.Unlock()

	storeDir := filepath.Join(m.baseDir, strconv.FormatInt(pocStageStartHeight, 10))
	if err := os.RemoveAll(storeDir); err != nil {
		return fmt.Errorf("remove store dir: %w", err)
	}

	logging.Info("Pruned artifact store", types.PoC, "height", pocStageStartHeight)
	return nil
}

// Flush flushes all open stores to disk.
func (m *ManagedArtifactStore) Flush() error {
	m.mu.RLock()
	stores := make([]*ArtifactStore, 0, len(m.stores))
	for _, s := range m.stores {
		stores = append(stores, s)
	}
	m.mu.RUnlock()

	var errs []error
	for _, s := range stores {
		if err := s.Flush(); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("flush errors: %v", errs)
	}
	return nil
}

// StartPeriodicFlush flushes all open stores at the specified interval.
func (m *ManagedArtifactStore) StartPeriodicFlush(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.Flush(); err != nil {
					logging.Warn("Periodic artifact flush failed", types.PoC, "error", err)
				}
			}
		}
	}()
}

// Close stops the cleanup loop, flushes and closes all stores.
func (m *ManagedArtifactStore) Close() error {
	// Stop cleanup goroutine first
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var errs []error
	for epoch, store := range m.stores {
		if err := store.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close epoch %d: %w", epoch, err))
		}
	}
	m.stores = make(map[int64]*ArtifactStore)

	if len(errs) > 0 {
		return fmt.Errorf("close errors: %v", errs)
	}
	return nil
}

// ListStores returns sorted list of block heights with stores on disk.
func (m *ManagedArtifactStore) ListStores() ([]int64, error) {
	entries, err := os.ReadDir(m.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read base dir: %w", err)
	}

	var heights []int64
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		height, err := strconv.ParseInt(e.Name(), 10, 64)
		if err != nil {
			continue // skip non-numeric dirs
		}
		heights = append(heights, height)
	}

	sort.Slice(heights, func(i, j int) bool { return heights[i] < heights[j] })
	return heights, nil
}

func (m *ManagedArtifactStore) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cleanup()
		}
	}
}

func (m *ManagedArtifactStore) cleanup() {
	heights, err := m.ListStores()
	if err != nil {
		logging.Warn("Failed to list artifact stores for cleanup", types.PoC, "error", err)
		return
	}

	// Keep the newest retainCount stores
	if len(heights) <= m.retainCount {
		return
	}

	// Prune oldest stores sequentially (heights is sorted ascending, so oldest are first)
	toPrune := heights[:len(heights)-m.retainCount]
	for _, height := range toPrune {
		if err := m.PruneStore(height); err != nil {
			logging.Warn("Auto-prune artifact store failed", types.PoC, "height", height, "error", err)
		}
	}
}
