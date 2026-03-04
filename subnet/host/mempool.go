package host

import (
	"subnet/types"

	"google.golang.org/protobuf/proto"
)

// MempoolEntry tracks a host-proposed tx awaiting inclusion.
type MempoolEntry struct {
	Tx         *types.SubnetTx
	ProposedAt uint64 // nonce when proposed
}

// Mempool stores host-proposed txs that haven't been included in a diff yet.
type Mempool struct {
	entries []MempoolEntry
}

func NewMempool() *Mempool {
	return &Mempool{}
}

func (m *Mempool) Add(entry MempoolEntry) {
	m.entries = append(m.entries, entry)
}

// RemoveIncluded removes entries whose tx matches any tx in the diff
// (by proto equality). Works for any host-proposed tx type.
func (m *Mempool) RemoveIncluded(txs []*types.SubnetTx) {
	if len(txs) == 0 || len(m.entries) == 0 {
		return
	}
	kept := m.entries[:0]
	for _, e := range m.entries {
		found := false
		for _, tx := range txs {
			if proto.Equal(e.Tx, tx) {
				found = true
				break
			}
		}
		if !found {
			kept = append(kept, e)
		}
	}
	m.entries = kept
}

// HasStale returns true if any entry was proposed more than grace nonces ago.
func (m *Mempool) HasStale(currentNonce, grace uint64) bool {
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
	txs := make([]*types.SubnetTx, len(m.entries))
	for i, e := range m.entries {
		txs[i] = e.Tx
	}
	return txs
}

func (m *Mempool) Len() int {
	return len(m.entries)
}
