package pocartifacts

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const (
	// MaxLeafCount caps artifacts to ensure MMR size calculations stay within int limits.
	// mmrSizeForLeaves(n) = 2n - popcount(n), so 2n must fit in int32: n <= MaxInt32/2.
	MaxLeafCount = (1 << 30) - 1 // 1,073,741,823
)

var (
	ErrDuplicateNonce      = errors.New("duplicate nonce")
	ErrLeafIndexOutOfRange = errors.New("leaf index out of range")
	ErrStoreClosed         = errors.New("store is closed")
	ErrCapacityExceeded    = errors.New("store capacity exceeded")
)

// ArtifactStore provides append-only storage for PoC artifacts with MMR commitments.
//
// Uses 2 files on disk + in-memory MMR. On restart, MMR and nonce map are rebuilt
// by re-hashing the data file (~2-3 sec for 1M artifacts). For instant recovery,
// persist MMR nodes to artifacts.tree and nonces to nonces.log.
type ArtifactStore struct {
	mu     sync.RWMutex
	dir    string
	closed bool

	dataFile *os.File // artifacts.data: [LE32 len][LE32 nonce][vector]...
	idxFile  *os.File // artifacts.index: [LE64 offset]... (entry k at byte k*8)

	buffer           []bufferedArtifact
	nonceToLeafIndex map[int32]uint32
	mmrNodes         [][]byte
	nextLeafIndex    uint32

	flushedLeafCount  uint32
	flushedDataOffset uint64
}

type bufferedArtifact struct {
	nonce  int32
	vector []byte
}

func Open(dir string) (*ArtifactStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create dir: %w", err)
	}

	dataPath := filepath.Join(dir, "artifacts.data")
	idxPath := filepath.Join(dir, "artifacts.index")

	dataFile, err := os.OpenFile(dataPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open data file: %w", err)
	}

	idxFile, err := os.OpenFile(idxPath, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		dataFile.Close()
		return nil, fmt.Errorf("open index file: %w", err)
	}

	s := &ArtifactStore{
		dir:              dir,
		dataFile:         dataFile,
		idxFile:          idxFile,
		buffer:           make([]bufferedArtifact, 0, 1024),
		nonceToLeafIndex: make(map[int32]uint32),
		mmrNodes:         make([][]byte, 0, 1024),
	}

	if err := s.recover(); err != nil {
		s.dataFile.Close()
		s.idxFile.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}

	return s, nil
}

func (s *ArtifactStore) recover() error {
	info, err := s.dataFile.Stat()
	if err != nil {
		return fmt.Errorf("stat data file: %w", err)
	}

	if info.Size() == 0 {
		return nil
	}

	if _, err := s.dataFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("seek data file: %w", err)
	}

	var offset uint64
	for {
		nonce, vector, n, err := readArtifact(s.dataFile)
		if err == io.EOF {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// Truncated record from crash - discard partial data
			if truncErr := s.truncateToOffset(offset); truncErr != nil {
				return fmt.Errorf("truncate after partial record: %w", truncErr)
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read artifact at offset %d: %w", offset, err)
		}

		if _, exists := s.nonceToLeafIndex[nonce]; exists {
			return fmt.Errorf("duplicate nonce %d at offset %d", nonce, offset)
		}

		if s.nextLeafIndex >= MaxLeafCount {
			return fmt.Errorf("data file exceeds max leaf count %d", MaxLeafCount)
		}

		s.nonceToLeafIndex[nonce] = s.nextLeafIndex
		leafHash := hashLeaf(encodeLeaf(nonce, vector))
		appendToMMR(&s.mmrNodes, leafHash, s.nextLeafIndex)
		s.nextLeafIndex++
		offset += uint64(n)
	}

	s.flushedLeafCount = s.nextLeafIndex
	s.flushedDataOffset = offset

	return nil
}

func (s *ArtifactStore) truncateToOffset(dataOffset uint64) error {
	if err := s.dataFile.Truncate(int64(dataOffset)); err != nil {
		return fmt.Errorf("truncate data file: %w", err)
	}
	idxSize := int64(s.nextLeafIndex) * 8
	if err := s.idxFile.Truncate(idxSize); err != nil {
		return fmt.Errorf("truncate index file: %w", err)
	}
	return nil
}

// Add appends an artifact if nonce is not already in the store.
func (s *ArtifactStore) Add(nonce int32, vector []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	if s.nextLeafIndex >= MaxLeafCount {
		return ErrCapacityExceeded
	}

	if _, exists := s.nonceToLeafIndex[nonce]; exists {
		return ErrDuplicateNonce
	}

	for _, b := range s.buffer {
		if b.nonce == nonce {
			return ErrDuplicateNonce
		}
	}

	s.nonceToLeafIndex[nonce] = s.nextLeafIndex
	s.buffer = append(s.buffer, bufferedArtifact{nonce: nonce, vector: vector})

	leafHash := hashLeaf(encodeLeaf(nonce, vector))
	appendToMMR(&s.mmrNodes, leafHash, s.nextLeafIndex)
	s.nextLeafIndex++

	return nil
}

func (s *ArtifactStore) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrStoreClosed
	}

	return s.flushLocked()
}

// flushLocked flushes buffered artifacts to disk. Caller must hold s.mu.
func (s *ArtifactStore) flushLocked() error {
	if len(s.buffer) == 0 {
		return nil
	}

	if _, err := s.dataFile.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek data file: %w", err)
	}

	if _, err := s.idxFile.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek index file: %w", err)
	}

	offset := s.flushedDataOffset
	for _, art := range s.buffer {
		var idxBuf [8]byte
		binary.LittleEndian.PutUint64(idxBuf[:], offset)
		if _, err := s.idxFile.Write(idxBuf[:]); err != nil {
			return fmt.Errorf("write index: %w", err)
		}

		n, err := writeArtifact(s.dataFile, art.nonce, art.vector)
		if err != nil {
			return fmt.Errorf("write artifact: %w", err)
		}
		offset += uint64(n)
	}

	if err := s.dataFile.Sync(); err != nil {
		return fmt.Errorf("sync data file: %w", err)
	}
	if err := s.idxFile.Sync(); err != nil {
		return fmt.Errorf("sync index file: %w", err)
	}

	s.flushedLeafCount = s.nextLeafIndex
	s.flushedDataOffset = offset
	s.buffer = s.buffer[:0]

	return nil
}

// GetRoot returns the current MMR root hash (32 bytes), or nil if empty.
func (s *ArtifactStore) GetRoot() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.nextLeafIndex == 0 {
		return nil
	}

	return bagPeaks(s.mmrNodes, s.nextLeafIndex)
}

func (s *ArtifactStore) Count() uint32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nextLeafIndex
}

func (s *ArtifactStore) GetArtifact(leafIndex uint32) (nonce int32, vector []byte, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return 0, nil, ErrStoreClosed
	}

	if leafIndex >= s.nextLeafIndex {
		return 0, nil, ErrLeafIndexOutOfRange
	}

	if leafIndex >= s.flushedLeafCount {
		bufIdx := leafIndex - s.flushedLeafCount
		art := s.buffer[bufIdx]
		return art.nonce, art.vector, nil
	}

	offset, err := s.readOffset(leafIndex)
	if err != nil {
		return 0, nil, fmt.Errorf("read offset: %w", err)
	}

	if _, err := s.dataFile.Seek(int64(offset), io.SeekStart); err != nil {
		return 0, nil, fmt.Errorf("seek data file: %w", err)
	}

	nonce, vector, _, err = readArtifact(s.dataFile)
	if err != nil {
		return 0, nil, fmt.Errorf("read artifact: %w", err)
	}

	return nonce, vector, nil
}

// GetProof generates a merkle proof for leafIndex at snapshotCount.
func (s *ArtifactStore) GetProof(leafIndex uint32, snapshotCount uint32) ([][]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.closed {
		return nil, ErrStoreClosed
	}

	if leafIndex >= snapshotCount {
		return nil, ErrLeafIndexOutOfRange
	}

	if snapshotCount > s.nextLeafIndex {
		return nil, fmt.Errorf("snapshot count %d exceeds current count %d", snapshotCount, s.nextLeafIndex)
	}

	return generateProof(s.mmrNodes, leafIndex, snapshotCount)
}

func (s *ArtifactStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}

	s.closed = true

	// Use internal flush (no lock) since we already hold the lock
	if err := s.flushLocked(); err != nil {
		return fmt.Errorf("flush on close: %w", err)
	}

	var errs []error
	if err := s.dataFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close data file: %w", err))
	}
	if err := s.idxFile.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close index file: %w", err))
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (s *ArtifactStore) readOffset(leafIndex uint32) (uint64, error) {
	var buf [8]byte
	pos := int64(leafIndex) * 8
	if _, err := s.idxFile.ReadAt(buf[:], pos); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

// writeArtifact format: [LE32 len][LE32 nonce][vector bytes]
func writeArtifact(w io.Writer, nonce int32, vector []byte) (int, error) {
	totalLen := 4 + len(vector)
	header := make([]byte, 8)
	binary.LittleEndian.PutUint32(header[0:4], uint32(totalLen))
	binary.LittleEndian.PutUint32(header[4:8], uint32(nonce))

	n1, err := w.Write(header)
	if err != nil {
		return n1, err
	}

	n2, err := w.Write(vector)
	if err != nil {
		return n1 + n2, err
	}

	return n1 + n2, nil
}

func readArtifact(r io.Reader) (int32, []byte, int, error) {
	var header [8]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, 0, err
	}

	totalLen := binary.LittleEndian.Uint32(header[0:4])
	nonce := int32(binary.LittleEndian.Uint32(header[4:8]))

	vectorLen := totalLen - 4
	vector := make([]byte, vectorLen)
	if _, err := io.ReadFull(r, vector); err != nil {
		return 0, nil, 0, err
	}

	return nonce, vector, 8 + int(vectorLen), nil
}

// encodeLeaf: LE32(nonce) || vector
func encodeLeaf(nonce int32, vector []byte) []byte {
	buf := make([]byte, 4+len(vector))
	binary.LittleEndian.PutUint32(buf[:4], uint32(nonce))
	copy(buf[4:], vector)
	return buf
}
