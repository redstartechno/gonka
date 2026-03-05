package host

import (
	"crypto/sha256"

	"google.golang.org/protobuf/proto"

	"subnet/types"
)

// MempoolEntry tracks a host-proposed tx awaiting inclusion.
type MempoolEntry struct {
	Tx         *types.SubnetTx
	ProposedAt uint64 // nonce when proposed
}

// Mempool stores host-proposed txs that haven't been included in a diff yet.
// Keyed by txHash for O(1) lookup and O(m) removal.
type Mempool struct {
	entries map[uint64]MempoolEntry
}

func NewMempool() *Mempool {
	return &Mempool{entries: make(map[uint64]MempoolEntry)}
}

func (m *Mempool) Add(entry MempoolEntry) {
	m.entries[txHash(entry.Tx)] = entry
}

// RemoveIncluded removes entries whose tx matches any tx in the diff (by hash).
func (m *Mempool) RemoveIncluded(txs []*types.SubnetTx) {
	for _, tx := range txs {
		delete(m.entries, txHash(tx))
	}
}

// HasStaleEntry returns true if any entry was proposed more than grace nonces ago.
// This is a pure data query with no signing decision.
func (m *Mempool) HasStaleEntry(currentNonce, grace uint64) bool {
	for _, e := range m.entries {
		if e.ProposedAt+grace < currentNonce {
			return true
		}
	}
	return false
}

func (m *Mempool) Txs() []*types.SubnetTx {
	if len(m.entries) == 0 {
		return nil
	}
	txs := make([]*types.SubnetTx, 0, len(m.entries))
	for _, e := range m.entries {
		txs = append(txs, e.Tx)
	}
	return txs
}

func (m *Mempool) Len() int {
	return len(m.entries)
}

// txHash computes a uint64 hash from the proto-serialized tx.
func txHash(tx *types.SubnetTx) uint64 {
	data, err := proto.Marshal(tx)
	if err != nil {
		return 0
	}
	h := sha256.Sum256(data)
	var v uint64
	for i := 0; i < 8; i++ {
		v = (v << 8) | uint64(h[i])
	}
	return v
}
