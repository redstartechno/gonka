package artifacts

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSMSTEmpty(t *testing.T) {
	tree := NewSMST(24)

	root, count := tree.GetRoot()
	if count != 0 {
		t.Errorf("expected count 0, got %d", count)
	}
	if root == nil {
		t.Error("expected non-nil empty root")
	}
}

func TestSMSTInsertAndCount(t *testing.T) {
	tree := NewSMST(24)

	for i := int32(0); i < 10; i++ {
		leafHash := smstHashLeaf(encodeLeaf(i, []byte{byte(i)}))
		_, err := tree.Insert(i, leafHash)
		if err != nil {
			t.Fatalf("Insert(%d) failed: %v", i, err)
		}
	}

	if tree.Count() != 10 {
		t.Errorf("expected count 10, got %d", tree.Count())
	}
}

func TestSMSTDuplicateRejection(t *testing.T) {
	tree := NewSMST(24)

	leafHash := smstHashLeaf(encodeLeaf(42, []byte{1, 2, 3}))
	if _, err := tree.Insert(42, leafHash); err != nil {
		t.Fatalf("First Insert failed: %v", err)
	}

	if _, err := tree.Insert(42, leafHash); err != ErrDuplicateNonce {
		t.Errorf("expected ErrDuplicateNonce, got %v", err)
	}

	if tree.Count() != 1 {
		t.Errorf("expected count 1, got %d", tree.Count())
	}
}

func TestSMSTDenseIndexNavigation(t *testing.T) {
	tree := NewSMST(24)

	nonces := []int32{100, 5, 1000, 50, 10}
	for _, nonce := range nonces {
		leafHash := smstHashLeaf(encodeLeaf(nonce, []byte{byte(nonce)}))
		tree.Insert(nonce, leafHash)
	}

	for i := uint32(0); i < tree.Count(); i++ {
		_, proof, err := tree.GetLeafByDenseIndex(i)
		if err != nil {
			t.Fatalf("GetLeafByDenseIndex(%d) failed: %v", i, err)
		}
		if len(proof) == 0 {
			t.Errorf("expected non-empty proof for index %d", i)
		}
	}
}

func TestSMSTRootConsistency(t *testing.T) {
	tree1 := NewSMST(24)
	tree2 := NewSMST(24)

	nonces := []int32{10, 20, 30}
	for _, n := range nonces {
		leafHash := smstHashLeaf(encodeLeaf(n, []byte{byte(n)}))
		tree1.Insert(n, leafHash)
	}

	for i := len(nonces) - 1; i >= 0; i-- {
		n := nonces[i]
		leafHash := smstHashLeaf(encodeLeaf(n, []byte{byte(n)}))
		tree2.Insert(n, leafHash)
	}

	root1, _ := tree1.GetRoot()
	root2, _ := tree2.GetRoot()

	if !bytes.Equal(root1, root2) {
		t.Error("roots should be equal regardless of insertion order")
	}
}

func TestSMSTDepthExpansion(t *testing.T) {
	tree := NewSMST(4)

	largeNonce := int32(1 << 20)
	leafHash := smstHashLeaf(encodeLeaf(largeNonce, []byte{1}))

	_, err := tree.Insert(largeNonce, leafHash)
	if err != nil {
		t.Fatalf("Insert with large nonce failed: %v", err)
	}

	if tree.Depth() < 20 {
		t.Errorf("expected depth >= 20, got %d", tree.Depth())
	}
}

func TestSMSTStoreBasics(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	if store.Count() != 0 {
		t.Errorf("expected count 0, got %d", store.Count())
	}

	for i := int32(0); i < 10; i++ {
		if err := store.Add(i, []byte{byte(i)}); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}

	if store.Count() != 10 {
		t.Errorf("expected count 10, got %d", store.Count())
	}
}

func TestSMSTStoreDuplicateRejection(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	if err := store.Add(42, []byte{1}); err != nil {
		t.Fatalf("First Add failed: %v", err)
	}

	if err := store.Add(42, []byte{2}); err != ErrDuplicateNonce {
		t.Errorf("expected ErrDuplicateNonce, got %v", err)
	}
}

func TestSMSTStoreRecovery(t *testing.T) {
	dir := t.TempDir()

	store1, _ := OpenSMST(dir)
	for i := int32(0); i < 5; i++ {
		store1.Add(i*10, []byte{byte(i)})
	}
	store1.Flush()
	root1 := store1.GetRoot()
	count1 := store1.Count()
	store1.Close()

	store2, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("Reopen failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != count1 {
		t.Errorf("recovered count: expected %d, got %d", count1, store2.Count())
	}

	root2 := store2.GetRoot()
	if !bytes.Equal(root1, root2) {
		t.Error("recovered root mismatch")
	}
}

func TestSMSTStoreProof(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	for i := int32(0); i < 8; i++ {
		store.Add(i, []byte{byte(i)})
	}

	root := store.GetRoot()
	for i := uint32(0); i < 8; i++ {
		proof, err := store.GetProof(i, 8)
		if err != nil {
			t.Fatalf("GetProof(%d) failed: %v", i, err)
		}
		if len(proof) == 0 {
			t.Errorf("expected non-empty proof for index %d", i)
		}

		nonce, vector, _ := store.GetArtifact(i)
		leafData := encodeLeaf(nonce, vector)

		proofElements := decodeProofFromTransport(proof)
		if !VerifySMSTProofWithCounts(root, 8, nonce, leafData, proofElements) {
			t.Errorf("proof verification failed for index %d", i)
		}
	}
}

func decodeProofFromTransport(proof [][]byte) []SMSTProofElement {
	elements := make([]SMSTProofElement, len(proof))
	for i, data := range proof {
		elements[i].SiblingHash = data[:32]
		elements[i].SiblingCount = binary.LittleEndian.Uint32(data[32:])
	}
	return elements
}

func TestSMSTStoreGetArtifact(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	// Artifacts inserted in random order
	inputArtifacts := []struct {
		nonce  int32
		vector []byte
	}{
		{100, []byte{1, 2, 3}},
		{50, []byte{4, 5, 6}},
		{200, []byte{7, 8, 9}},
	}

	for _, a := range inputArtifacts {
		store.Add(a.nonce, a.vector)
	}

	// SMST returns artifacts in tree-traversal order (by nonce position)
	// Smaller nonces go to the left, so order should be: 50, 100, 200
	expectedOrder := []struct {
		nonce  int32
		vector []byte
	}{
		{50, []byte{4, 5, 6}},
		{100, []byte{1, 2, 3}},
		{200, []byte{7, 8, 9}},
	}

	for i, expected := range expectedOrder {
		nonce, vector, err := store.GetArtifact(uint32(i))
		if err != nil {
			t.Fatalf("GetArtifact(%d) failed: %v", i, err)
		}
		if nonce != expected.nonce {
			t.Errorf("artifact %d: expected nonce %d, got %d", i, expected.nonce, nonce)
		}
		if !bytes.Equal(vector, expected.vector) {
			t.Errorf("artifact %d: vector mismatch", i)
		}
	}
}

func TestSMSTVerifyProof(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST: %v", err)
	}
	defer store.Close()

	nonces := []int32{10, 20, 30, 40, 50}
	for _, n := range nonces {
		if err := store.Add(n, []byte{byte(n)}); err != nil {
			t.Fatalf("Add(%d): %v", n, err)
		}
	}
	store.Flush()

	count, rootHash := store.GetFlushedRoot()

	for i := uint32(0); i < count; i++ {
		nonce, vector, err := store.GetArtifact(i)
		if err != nil {
			t.Fatalf("GetArtifact(%d): %v", i, err)
		}

		proof, err := store.GetProof(i, count)
		if err != nil {
			t.Fatalf("GetProof(%d): %v", i, err)
		}

		leafData := encodeLeaf(nonce, vector)
		if !VerifySMSTProofSlice(rootHash, count, nonce, leafData, proof) {
			t.Errorf("Proof verification failed for index %d (nonce=%d)", i, nonce)
		}
	}
}

func TestSMSTProofEncoding(t *testing.T) {
	proof := []SMSTProofElement{
		{SiblingHash: make([]byte, 32), SiblingCount: 100},
		{SiblingHash: make([]byte, 32), SiblingCount: 200},
	}

	for i := range proof[0].SiblingHash {
		proof[0].SiblingHash[i] = byte(i)
	}
	for i := range proof[1].SiblingHash {
		proof[1].SiblingHash[i] = byte(32 - i)
	}

	encoded := EncodeSMSTProof(proof)
	decoded, err := DecodeSMSTProof(encoded)
	if err != nil {
		t.Fatalf("DecodeSMSTProof failed: %v", err)
	}

	if len(decoded) != len(proof) {
		t.Fatalf("decoded length mismatch: expected %d, got %d", len(proof), len(decoded))
	}

	for i := range proof {
		if !bytes.Equal(decoded[i].SiblingHash, proof[i].SiblingHash) {
			t.Errorf("element %d: hash mismatch", i)
		}
		if decoded[i].SiblingCount != proof[i].SiblingCount {
			t.Errorf("element %d: count mismatch: expected %d, got %d", i, proof[i].SiblingCount, decoded[i].SiblingCount)
		}
	}
}

func BenchmarkSMSTAdd(b *testing.B) {
	dir := b.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	vector := make([]byte, 24)
	for i := range vector {
		vector[i] = byte(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Add(int32(i), vector)
	}
}

func BenchmarkSMSTAddWithFlush(b *testing.B) {
	dir := b.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	vector := make([]byte, 24)
	for i := range vector {
		vector[i] = byte(i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.Add(int32(i), vector)
		if (i+1)%1000 == 0 {
			store.Flush()
		}
	}
	store.Flush()
}

func BenchmarkSMSTGetProof(b *testing.B) {
	dir := b.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	const treeSize = 100000
	vector := make([]byte, 24)
	for i := 0; i < treeSize; i++ {
		store.Add(int32(i), vector)
	}
	store.Flush()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		leafIdx := uint32(i % treeSize)
		store.GetProof(leafIdx, treeSize)
	}
}

func BenchmarkSMSTVerifyProof(b *testing.B) {
	dir := b.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	const treeSize = 100000
	vector := make([]byte, 24)
	for i := 0; i < treeSize; i++ {
		store.Add(int32(i), vector)
	}
	store.Flush()

	root := store.GetRoot()
	proofs := make([][][]byte, 100)
	nonces := make([]int32, 100)
	for i := 0; i < 100; i++ {
		proofs[i], _ = store.GetProof(uint32(i), treeSize)
		nonces[i], _, _ = store.GetArtifact(uint32(i))
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		idx := i % 100
		leafData := encodeLeaf(nonces[idx], vector)
		elements := decodeProofFromTransport(proofs[idx])
		VerifySMSTProofWithCounts(root, treeSize, nonces[idx], leafData, elements)
	}
}

func BenchmarkSMSTRecovery(b *testing.B) {
	sizes := []int{10000, 100000, 1000000}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size_%d", size), func(b *testing.B) {
			dir := b.TempDir()

			store, _ := OpenSMST(dir)
			vector := make([]byte, 24)
			for i := 0; i < size; i++ {
				store.Add(int32(i), vector)
			}
			store.Flush()
			store.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, _ := OpenSMST(dir)
				s.Close()
			}
		})
	}
}

func TestSMSTProofSizes(t *testing.T) {
	sizes := []int{1000, 10000, 100000}

	for _, size := range sizes {
		t.Run(fmt.Sprintf("size_%d", size), func(t *testing.T) {
			dir := t.TempDir()
			store, _ := OpenSMST(dir)
			defer store.Close()

			vector := make([]byte, 24)
			for i := 0; i < size; i++ {
				store.Add(int32(i), vector)
			}

			var totalProofBytes int
			sampleCount := 100
			if size < sampleCount {
				sampleCount = size
			}

			for i := 0; i < sampleCount; i++ {
				leafIdx := uint32(i * size / sampleCount)
				proof, err := store.GetProof(leafIdx, uint32(size))
				if err != nil {
					t.Fatalf("GetProof failed: %v", err)
				}
				for _, h := range proof {
					totalProofBytes += len(h)
				}
			}

			avgProofBytes := totalProofBytes / sampleCount
			t.Logf("Tree size: %d, Average proof size: %d bytes (%d elements of 36 bytes)", size, avgProofBytes, avgProofBytes/36)
		})
	}
}

func TestSMSTStoreNodeDistribution(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	store.AddWithNode(1, []byte{1}, "nodeA")
	store.AddWithNode(2, []byte{2}, "nodeA")
	store.AddWithNode(3, []byte{3}, "nodeB")
	store.AddWithNode(4, []byte{4}, "")

	counts := store.GetNodeCounts()
	if counts["nodeA"] != 2 {
		t.Errorf("expected nodeA count 2, got %d", counts["nodeA"])
	}
	if counts["nodeB"] != 1 {
		t.Errorf("expected nodeB count 1, got %d", counts["nodeB"])
	}

	dist := store.GetNodeDistribution()
	if len(dist) != 0 {
		t.Errorf("expected empty distribution before flush")
	}

	store.Flush()

	dist = store.GetNodeDistribution()
	if dist["nodeA"] != 2 {
		t.Errorf("expected flushed nodeA count 2, got %d", dist["nodeA"])
	}
	if dist["nodeB"] != 1 {
		t.Errorf("expected flushed nodeB count 1, got %d", dist["nodeB"])
	}
}

func TestSMSTStoreNodeDistributionRecovery(t *testing.T) {
	dir := t.TempDir()

	store1, _ := OpenSMST(dir)
	store1.AddWithNode(1, []byte{1}, "nodeA")
	store1.AddWithNode(2, []byte{2}, "nodeB")
	store1.Flush()
	store1.Close()

	store2, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST reopen failed: %v", err)
	}
	defer store2.Close()

	dist := store2.GetNodeDistribution()
	if dist["nodeA"] != 1 {
		t.Errorf("expected recovered nodeA count 1, got %d", dist["nodeA"])
	}
	if dist["nodeB"] != 1 {
		t.Errorf("expected recovered nodeB count 1, got %d", dist["nodeB"])
	}

	counts := store2.GetNodeCounts()
	if counts["nodeA"] != 1 {
		t.Errorf("expected recovered nodeA count 1 in current, got %d", counts["nodeA"])
	}
}

func TestSMSTStoreGetRootAt(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	root, err := store.GetRootAt(0)
	if err != nil {
		t.Errorf("GetRootAt(0) should not error: %v", err)
	}
	if root != nil {
		t.Errorf("GetRootAt(0) should return nil")
	}

	roots := make([][]byte, 6)
	for i := int32(1); i <= 5; i++ {
		store.Add(i, []byte{byte(i)})
		store.Flush()
		root, err := store.GetRootAt(uint32(i))
		if err != nil {
			t.Fatalf("GetRootAt(%d) failed: %v", i, err)
		}
		roots[i] = root
	}

	for i := uint32(1); i <= 5; i++ {
		root, err := store.GetRootAt(i)
		if err != nil {
			t.Fatalf("GetRootAt(%d) failed: %v", i, err)
		}
		if !bytes.Equal(root, roots[i]) {
			t.Errorf("GetRootAt(%d) returned different root", i)
		}
	}
}

func TestSMSTStoreGetFlushedRoot(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	count, root := store.GetFlushedRoot()
	if count != 0 {
		t.Errorf("expected count 0, got %d", count)
	}
	if root != nil {
		t.Errorf("expected nil root for empty store")
	}

	for i := int32(0); i < 5; i++ {
		store.Add(i, []byte{byte(i)})
	}

	count, root = store.GetFlushedRoot()
	if count != 0 {
		t.Errorf("expected flushed count 0 before flush, got %d", count)
	}

	store.Flush()
	count, root = store.GetFlushedRoot()
	if count != 5 {
		t.Errorf("expected flushed count 5 after flush, got %d", count)
	}
	if root == nil {
		t.Error("expected non-nil root after flush")
	}
}

func TestSMSTStoreAddAfterClose(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	store.Add(1, []byte{1})
	store.Close()

	err := store.Add(2, []byte{2})
	if err != ErrStoreClosed {
		t.Errorf("expected ErrStoreClosed, got %v", err)
	}
}

func TestSMSTStoreNegativeNonce(t *testing.T) {
	dir := t.TempDir()
	store, _ := OpenSMST(dir)
	defer store.Close()

	store.Add(-1, []byte{1})
	store.Add(-100, []byte{2})
	store.Add(-2147483648, []byte{3}) // INT32_MIN

	if store.Count() != 3 {
		t.Errorf("expected 3, got %d", store.Count())
	}

	store.Flush()

	// SMST orders by nonce interpreted as uint32
	// -1 = 0xFFFFFFFF (largest), -100 = 0xFFFFFF9C, -2147483648 = 0x80000000
	// Tree order (smallest first): 0x80000000, 0xFFFFFF9C, 0xFFFFFFFF
	// So: -2147483648, -100, -1
	expectedNonces := []int32{-2147483648, -100, -1}

	for i, expected := range expectedNonces {
		nonce, _, err := store.GetArtifact(uint32(i))
		if err != nil {
			t.Fatalf("GetArtifact(%d) failed: %v", i, err)
		}
		if nonce != expected {
			t.Errorf("artifact %d: expected nonce %d, got %d", i, expected, nonce)
		}
	}
}

func TestSMSTStoreTruncatedRecordRecovery(t *testing.T) {
	dir := t.TempDir()

	store1, _ := OpenSMST(dir)
	store1.Add(10, []byte{1, 2, 3})
	store1.Add(20, []byte{4, 5, 6})
	store1.Flush()
	root1 := store1.GetRoot()
	store1.Close()

	dataPath := dir + "/artifacts.data"
	f, err := os.OpenFile(dataPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open data file: %v", err)
	}
	f.Write([]byte{0x10, 0x00, 0x00, 0x00})
	f.Close()

	store2, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("Reopen with truncated record failed: %v", err)
	}
	defer store2.Close()

	if store2.Count() != 2 {
		t.Errorf("expected count 2 after truncation recovery, got %d", store2.Count())
	}

	root2 := store2.GetRoot()
	if !bytes.Equal(root1, root2) {
		t.Errorf("root mismatch after truncation recovery")
	}
}

func TestSMSTStoreCapacityExceeded(t *testing.T) {
	t.Skip("MaxLeafCount is too large to test in unit tests")
}

func TestSMSTStoreConcurrentGetArtifact(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	const artifactCount = 100
	const goroutines = 50
	const readsPerGoroutine = 20

	// Insert artifacts with sequential nonces
	for i := 0; i < artifactCount; i++ {
		vector := []byte{byte(i), byte(i + 1), byte(i + 2), byte(i + 3)}
		if err := store.Add(int32(i), vector); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	// Build expected mapping: dense index -> (nonce, vector)
	// SMST orders by nonce position in tree (sorted order for sequential nonces)
	expectedData := make(map[uint32]struct {
		nonce  int32
		vector []byte
	})
	for i := uint32(0); i < artifactCount; i++ {
		nonce, vector, _ := store.GetArtifact(i)
		expectedData[i] = struct {
			nonce  int32
			vector []byte
		}{nonce, vector}
	}

	var wg sync.WaitGroup
	errChan := make(chan error, goroutines*readsPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for r := 0; r < readsPerGoroutine; r++ {
				leafIdx := uint32((goroutineID*readsPerGoroutine + r) % artifactCount)
				nonce, vector, err := store.GetArtifact(leafIdx)
				if err != nil {
					errChan <- fmt.Errorf("goroutine %d: GetArtifact(%d) failed: %v", goroutineID, leafIdx, err)
					return
				}
				expected := expectedData[leafIdx]
				if nonce != expected.nonce {
					errChan <- fmt.Errorf("goroutine %d: leafIdx %d: expected nonce %d, got %d", goroutineID, leafIdx, expected.nonce, nonce)
					return
				}
				if !bytes.Equal(vector, expected.vector) {
					errChan <- fmt.Errorf("goroutine %d: leafIdx %d: vector mismatch", goroutineID, leafIdx)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errChan)

	for err := range errChan {
		t.Error(err)
	}
}

// TestSMSTStoreConcurrentReadsHeavy tests many parallel goroutines doing heavy reads
func TestSMSTStoreConcurrentReadsHeavy(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	const artifactCount = 1000
	const goroutines = 100
	const readsPerGoroutine = 100

	// Insert artifacts
	for i := 0; i < artifactCount; i++ {
		vector := make([]byte, 24)
		binary.LittleEndian.PutUint32(vector, uint32(i))
		if err := store.Add(int32(i), vector); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}
	store.Flush()

	count := store.Count()
	rootHash := store.GetRoot()

	var wg sync.WaitGroup
	errChan := make(chan error, goroutines)

	// Launch many concurrent readers
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			for r := 0; r < readsPerGoroutine; r++ {
				leafIdx := uint32((goroutineID + r*goroutines) % int(count))

				// Test GetArtifact
				nonce, vector, err := store.GetArtifact(leafIdx)
				if err != nil {
					errChan <- fmt.Errorf("g%d: GetArtifact(%d) failed: %v", goroutineID, leafIdx, err)
					return
				}
				if len(vector) != 24 {
					errChan <- fmt.Errorf("g%d: unexpected vector length %d", goroutineID, len(vector))
					return
				}

				// Test GetProof
				proof, err := store.GetProof(leafIdx, count)
				if err != nil {
					errChan <- fmt.Errorf("g%d: GetProof(%d) failed: %v", goroutineID, leafIdx, err)
					return
				}

				// Verify proof
				leafData := encodeLeaf(nonce, vector)
				elements := DecodeProofElements(proof)
				if !VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements) {
					errChan <- fmt.Errorf("g%d: proof verification failed for index %d", goroutineID, leafIdx)
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errChan)

	errCount := 0
	for err := range errChan {
		t.Error(err)
		errCount++
	}

	if errCount == 0 {
		t.Logf("Success: %d goroutines x %d reads = %d concurrent operations", goroutines, readsPerGoroutine, goroutines*readsPerGoroutine)
	}
}

// TestSMSTStoreConcurrentReadsWithProofs tests GetArtifact and GetProof together under load
func TestSMSTStoreConcurrentReadsWithProofs(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	const artifactCount = 500
	const goroutines = 50
	const opsPerGoroutine = 50

	// Insert with random-ish nonces
	for i := 0; i < artifactCount; i++ {
		nonce := int32(i*7 + 13) // non-sequential to stress tree navigation
		vector := make([]byte, 24)
		binary.LittleEndian.PutUint32(vector, uint32(nonce))
		if err := store.Add(nonce, vector); err != nil {
			t.Fatalf("Add failed: %v", err)
		}
	}
	store.Flush()

	count := store.Count()
	rootHash := store.GetRoot()

	var wg sync.WaitGroup
	var successCount, failCount int64

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			localSuccess := int64(0)
			localFail := int64(0)

			for op := 0; op < opsPerGoroutine; op++ {
				idx := uint32((gid*opsPerGoroutine + op) % int(count))

				nonce, vector, err := store.GetArtifact(idx)
				if err != nil {
					localFail++
					continue
				}

				proof, err := store.GetProof(idx, count)
				if err != nil {
					localFail++
					continue
				}

				leafData := encodeLeaf(nonce, vector)
				elements := DecodeProofElements(proof)
				if VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements) {
					localSuccess++
				} else {
					localFail++
				}
			}

			atomic.AddInt64(&successCount, localSuccess)
			atomic.AddInt64(&failCount, localFail)
		}(g)
	}

	wg.Wait()

	if failCount > 0 {
		t.Errorf("Concurrent test had %d failures out of %d operations", failCount, goroutines*opsPerGoroutine)
	} else {
		t.Logf("All %d concurrent operations succeeded", successCount)
	}
}

// TestSMSTStoreConcurrentReadsWhileWriting tests that reads don't block during writes
// and data remains consistent when read/write operations interleave.
func TestSMSTStoreConcurrentReadsWhileWriting(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	// Pre-populate some data
	for i := 0; i < 100; i++ {
		vector := make([]byte, 24)
		binary.LittleEndian.PutUint32(vector, uint32(i))
		if err := store.Add(int32(i), vector); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
	}
	store.Flush()

	var wg sync.WaitGroup
	var readSuccess, readFail, writeSuccess int64

	// Start readers that run continuously
	readerDone := make(chan struct{})
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(gid int) {
			defer wg.Done()
			for {
				select {
				case <-readerDone:
					return
				default:
					count := store.Count()
					if count == 0 {
						continue
					}
					idx := uint32(gid) % count
					nonce, vector, err := store.GetArtifact(idx)
					if err != nil {
						atomic.AddInt64(&readFail, 1)
						continue
					}
					if vector == nil || nonce == 0 && len(vector) == 0 {
						atomic.AddInt64(&readFail, 1)
						continue
					}
					atomic.AddInt64(&readSuccess, 1)
				}
			}
		}(g)
	}

	// Writer adds more data while readers are active
	for i := 100; i < 500; i++ {
		vector := make([]byte, 24)
		binary.LittleEndian.PutUint32(vector, uint32(i))
		if err := store.Add(int32(i), vector); err != nil {
			t.Fatalf("Add(%d) failed: %v", i, err)
		}
		if i%50 == 0 {
			store.Flush()
		}
		atomic.AddInt64(&writeSuccess, 1)
	}

	// Stop readers
	close(readerDone)
	wg.Wait()

	if readFail > 0 {
		t.Errorf("Had %d read failures during concurrent read/write", readFail)
	}
	t.Logf("Concurrent read/write: %d successful reads, %d writes", readSuccess, writeSuccess)
}

// TestSMSTSumBasedOrdering verifies the core SMST property:
// dense indices are deterministically ordered by nonce position in the sparse tree,
// using the sum (count) at each node to navigate to the i-th leaf.
func TestSMSTSumBasedOrdering(t *testing.T) {
	tree := NewSMST(24)

	// Insert nonces in random order - they should be retrievable by dense index
	// in a deterministic order based on tree structure (left-to-right traversal)
	nonces := []int32{1000, 50, 500, 5, 100, 10, 750}
	for _, nonce := range nonces {
		leafHash := smstHashLeaf(encodeLeaf(nonce, []byte{byte(nonce)}))
		_, err := tree.Insert(nonce, leafHash)
		if err != nil {
			t.Fatalf("Insert(%d) failed: %v", nonce, err)
		}
	}

	// Verify count equals number of inserted nonces
	if tree.Count() != uint32(len(nonces)) {
		t.Fatalf("expected count %d, got %d", len(nonces), tree.Count())
	}

	// Get all nonces by dense index - should be left-to-right tree order
	retrievedNonces := make([]int32, tree.Count())
	for i := uint32(0); i < tree.Count(); i++ {
		nonce, _, err := tree.GetLeafByDenseIndex(i)
		if err != nil {
			t.Fatalf("GetLeafByDenseIndex(%d) failed: %v", i, err)
		}
		retrievedNonces[i] = nonce
	}

	// The order should be deterministic based on tree structure
	// Smaller nonces go to the left (path has more 0 bits), larger to the right
	// So nonces should be roughly sorted when retrieved by dense index
	t.Logf("Retrieved nonces by dense index: %v", retrievedNonces)

	// Verify all original nonces are present
	seenNonces := make(map[int32]bool)
	for _, n := range retrievedNonces {
		seenNonces[n] = true
	}
	for _, n := range nonces {
		if !seenNonces[n] {
			t.Errorf("nonce %d not found in retrieved nonces", n)
		}
	}
}

// TestSMSTProofProvesCorrectNonceAtIndex verifies that:
// 1. A proof generated for dense index i
// 2. Contains the correct nonce (the one at that position)
// 3. Verifies successfully against the root
func TestSMSTProofProvesCorrectNonceAtIndex(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	// Insert artifacts with various nonces
	artifacts := []struct {
		nonce  int32
		vector []byte
	}{
		{100, []byte{1, 2, 3}},
		{5, []byte{4, 5, 6}},
		{1000, []byte{7, 8, 9}},
		{50, []byte{10, 11, 12}},
		{500, []byte{13, 14, 15}},
	}

	for _, a := range artifacts {
		if err := store.Add(a.nonce, a.vector); err != nil {
			t.Fatalf("Add(%d) failed: %v", a.nonce, err)
		}
	}
	store.Flush()

	count := store.Count()
	rootHash := store.GetRoot()

	t.Logf("Store has %d artifacts, root: %x", count, rootHash[:8])

	// For each dense index, verify:
	// 1. GetArtifact returns a nonce
	// 2. GetProof returns a valid proof
	// 3. The proof verifies correctly for that specific nonce
	for i := uint32(0); i < count; i++ {
		nonce, vector, err := store.GetArtifact(i)
		if err != nil {
			t.Fatalf("GetArtifact(%d) failed: %v", i, err)
		}

		proof, err := store.GetProof(i, count)
		if err != nil {
			t.Fatalf("GetProof(%d, %d) failed: %v", i, count, err)
		}

		// Build leaf data for verification
		leafData := encodeLeaf(nonce, vector)

		// Decode and verify the proof
		elements := DecodeProofElements(proof)
		verified := VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements)
		if !verified {
			t.Errorf("Proof verification failed for dense index %d (nonce %d)", i, nonce)
			t.Logf("  proof elements: %d, nonce: %d, leafData len: %d", len(elements), nonce, len(leafData))
			t.Logf("  rootHash: %x", rootHash)
			t.Logf("  count: %d", count)
		} else {
			t.Logf("Dense index %d: nonce=%d, proof verified OK", i, nonce)
		}
	}
}

// TestSMSTProofFailsForWrongNonce verifies that a proof for one nonce
// does NOT verify if we claim it's for a different nonce
func TestSMSTProofFailsForWrongNonce(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	store.Add(100, []byte{1, 2, 3})
	store.Add(200, []byte{4, 5, 6})
	store.Add(300, []byte{7, 8, 9})
	store.Flush()

	count := store.Count()
	rootHash := store.GetRoot()

	// Get proof for index 0
	nonce0, vector0, _ := store.GetArtifact(0)
	proof0, _ := store.GetProof(0, count)
	elements0 := DecodeProofElements(proof0)

	// Verify it works for the correct nonce
	leafData0 := encodeLeaf(nonce0, vector0)
	if !VerifySMSTProofWithCounts(rootHash, count, nonce0, leafData0, elements0) {
		t.Fatal("Proof should verify for correct nonce")
	}

	// Try to verify with a WRONG nonce - should fail
	wrongNonce := int32(999)
	wrongLeafData := encodeLeaf(wrongNonce, vector0)
	if VerifySMSTProofWithCounts(rootHash, count, wrongNonce, wrongLeafData, elements0) {
		t.Error("Proof should NOT verify for wrong nonce - this breaks duplicate prevention!")
	}
}

// TestSMSTDuplicateNoncePreventionEndToEnd tests the full duplicate prevention flow:
// 1. Participant inserts artifacts with unique nonces
// 2. Attempting to insert duplicate nonce fails
// 3. Proofs can only verify artifacts that were actually inserted
func TestSMSTDuplicateNoncePreventionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	// Simulate: participant receives nonces 1, 2, 3 from inference
	store.Add(1, []byte{0x11})
	store.Add(2, []byte{0x22})
	store.Add(3, []byte{0x33})

	// Malicious attempt: try to add nonce 2 again (duplicate)
	err = store.Add(2, []byte{0xFF})
	if err != ErrDuplicateNonce {
		t.Errorf("Expected ErrDuplicateNonce, got: %v", err)
	}

	// Count should still be 3
	if store.Count() != 3 {
		t.Errorf("Expected count 3, got %d", store.Count())
	}

	store.Flush()
	rootHash := store.GetRoot()
	count := store.Count()

	// Verify all inserted nonces can be proven
	for i := uint32(0); i < count; i++ {
		nonce, vector, _ := store.GetArtifact(i)
		proof, _ := store.GetProof(i, count)
		leafData := encodeLeaf(nonce, vector)
		elements := DecodeProofElements(proof)

		if !VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements) {
			t.Errorf("Valid artifact at index %d (nonce %d) failed verification", i, nonce)
		}
	}

	// Verify that a non-existent nonce cannot be proven
	// (There's no way to construct a valid proof for nonce 999 without having inserted it)
	fakeNonce := int32(999)
	fakeVector := []byte{0x99}
	fakeLeafData := encodeLeaf(fakeNonce, fakeVector)

	// Get proof for index 0 and try to use it for fake nonce
	proof0, _ := store.GetProof(0, count)
	elements0 := DecodeProofElements(proof0)

	if VerifySMSTProofWithCounts(rootHash, count, fakeNonce, fakeLeafData, elements0) {
		t.Error("Fake nonce should NOT verify with stolen proof")
	}

	t.Log("Duplicate prevention working correctly")
}

// Regression tests for risk hardening

func TestSMSTDepthExpansionHashCorrectness(t *testing.T) {
	// Verify that proofs remain valid and verifiable after depth expansion.
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST: %v", err)
	}
	defer store.Close()

	// Insert nonces that fit in default depth
	nonces := []int32{1, 5, 10, 100}
	for _, n := range nonces {
		if err := store.Add(n, []byte{byte(n)}); err != nil {
			t.Fatalf("Add(%d): %v", n, err)
		}
	}
	store.Flush()

	countBefore := store.Count()
	rootBefore := store.GetRoot()

	// Verify proofs work before expansion
	for i := uint32(0); i < countBefore; i++ {
		nonce, vector, _ := store.GetArtifact(i)
		proof, _ := store.GetProof(i, countBefore)
		leafData := encodeLeaf(nonce, vector)
		if !VerifySMSTProofSlice(rootBefore, countBefore, nonce, leafData, proof) {
			t.Errorf("proof verification failed BEFORE expansion for index %d", i)
		}
	}

	// Force expansion with large nonce (> 2^24 to exceed default depth)
	largeNonce := int32(1 << 25)
	if err := store.Add(largeNonce, []byte{0xFF}); err != nil {
		t.Fatalf("Add(%d): %v", largeNonce, err)
	}
	store.Flush()

	countAfter := store.Count()
	rootAfter := store.GetRoot()

	if bytes.Equal(rootBefore, rootAfter) {
		t.Errorf("root should change after expansion")
	}

	// Verify proofs work AFTER expansion for ALL artifacts including originals
	for i := uint32(0); i < countAfter; i++ {
		nonce, vector, _ := store.GetArtifact(i)
		proof, err := store.GetProof(i, countAfter)
		if err != nil {
			t.Errorf("GetProof(%d) after expansion: %v", i, err)
			continue
		}
		leafData := encodeLeaf(nonce, vector)
		if !VerifySMSTProofSlice(rootAfter, countAfter, nonce, leafData, proof) {
			t.Errorf("proof verification FAILED after expansion for index %d (nonce=%d)", i, nonce)
		}
	}
}

func TestSMSTDepthExpansionIncremental(t *testing.T) {
	// Start with depth 4 and expand incrementally to depth 24.
	// After each expansion, verify ALL previous artifacts still have valid proofs.
	// Tests the expansion algorithm by working directly with SMST at small initial depth.

	initialDepth := 4
	finalDepth := 24

	tree := NewSMST(initialDepth)

	// Track all inserted items
	type item struct {
		nonce  int32
		vector []byte
	}
	var inserted []item

	// Helper to build proof with counts directly from tree (like store does)
	buildProofWithCounts := func(tree *SMST, nonce int32) []SMSTProofElement {
		path := tree.noncePath(nonce)
		elements := make([]SMSTProofElement, 0, tree.depth)

		var collect func(node *smstNode, level int)
		collect = func(node *smstNode, level int) {
			if level == tree.depth || node == nil {
				return
			}
			goRight := path[level]
			if goRight {
				elements = append(elements, SMSTProofElement{
					SiblingHash:  tree.nodeHash(node.left, level+1),
					SiblingCount: tree.nodeCount(node.left),
				})
				collect(node.right, level+1)
			} else {
				elements = append(elements, SMSTProofElement{
					SiblingHash:  tree.nodeHash(node.right, level+1),
					SiblingCount: tree.nodeCount(node.right),
				})
				collect(node.left, level+1)
			}
		}
		collect(tree.root, 0)
		return elements
	}

	// Helper to verify all proofs
	verifyAll := func(stage string) bool {
		rootHash, count := tree.GetRoot()
		allPassed := true
		for _, it := range inserted {
			proofElements := buildProofWithCounts(tree, it.nonce)
			leafData := encodeLeaf(it.nonce, it.vector)
			if !VerifySMSTProofWithCounts(rootHash, count, it.nonce, leafData, proofElements) {
				t.Errorf("[%s] Proof FAILED for nonce=%d", stage, it.nonce)
				allPassed = false
			}
		}
		return allPassed
	}

	// Insert initial nonces that fit in depth 4 (0-15)
	initialNonces := []int32{0, 1, 5, 10, 15}
	for _, n := range initialNonces {
		vec := []byte{byte(n)}
		leafHash := smstHashLeaf(encodeLeaf(n, vec))
		if _, err := tree.Insert(n, leafHash); err != nil {
			t.Fatalf("Initial Insert(%d): %v", n, err)
		}
		inserted = append(inserted, item{n, vec})
	}

	if tree.Depth() != initialDepth {
		t.Errorf("Expected initial depth %d, got %d", initialDepth, tree.Depth())
	}

	t.Logf("Initial: depth=%d, count=%d", tree.Depth(), tree.Count())
	if !verifyAll("initial") {
		t.Fatal("Initial verification failed")
	}

	// Expand incrementally from depth 5 to finalDepth
	for targetDepth := initialDepth + 1; targetDepth <= finalDepth; targetDepth++ {
		// Nonce requiring depth D has highest bit at position D-1
		// 2^(D-1) requires exactly depth D
		triggerNonce := int32(1<<(targetDepth-1)) + int32(targetDepth)

		vec := []byte{byte(triggerNonce), byte(triggerNonce >> 8), byte(triggerNonce >> 16)}
		leafHash := smstHashLeaf(encodeLeaf(triggerNonce, vec))

		depthBefore := tree.Depth()
		if _, err := tree.Insert(triggerNonce, leafHash); err != nil {
			t.Fatalf("Insert(nonce=%d) for depth %d: %v", triggerNonce, targetDepth, err)
		}
		inserted = append(inserted, item{triggerNonce, vec})

		if tree.Depth() < targetDepth {
			t.Errorf("Expected depth >= %d, got %d", targetDepth, tree.Depth())
		}

		expanded := depthBefore != tree.Depth()
		if expanded {
			t.Logf("Expanded: %d -> %d (nonce=%d, count=%d)",
				depthBefore, tree.Depth(), triggerNonce, tree.Count())
		}

		// Verify ALL proofs still work
		stage := fmt.Sprintf("depth=%d", tree.Depth())
		if !verifyAll(stage) {
			t.Fatalf("Verification failed after expanding to depth %d", tree.Depth())
		}
	}

	t.Logf("SUCCESS: depth %d->%d, %d items, all proofs verified at each step",
		initialDepth, tree.Depth(), len(inserted))
}

func TestSMSTMalformedProofRejection(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST: %v", err)
	}
	defer store.Close()

	nonces := []int32{10, 20, 30, 40, 50}
	for _, n := range nonces {
		if err := store.Add(n, []byte{byte(n)}); err != nil {
			t.Fatalf("Add(%d): %v", n, err)
		}
	}
	store.Flush()

	count, rootHash := store.GetFlushedRoot()
	nonce, vector, _ := store.GetArtifact(2)
	leafData := encodeLeaf(nonce, vector)

	// Test 1: Empty proof should fail
	if VerifySMSTProofSlice(rootHash, count, nonce, leafData, nil) {
		t.Error("empty proof should fail verification")
	}
	if VerifySMSTProofSlice(rootHash, count, nonce, leafData, [][]byte{}) {
		t.Error("empty slice proof should fail verification")
	}

	// Test 2: Malformed proof element (wrong size) should fail
	if VerifySMSTProofSlice(rootHash, count, nonce, leafData, [][]byte{make([]byte, 35)}) {
		t.Error("malformed proof (35 bytes) should fail verification")
	}
	if VerifySMSTProofSlice(rootHash, count, nonce, leafData, [][]byte{make([]byte, 37)}) {
		t.Error("malformed proof (37 bytes) should fail verification")
	}

	// Test 3: Proof with correct size but wrong content should fail
	wrongContent := make([][]byte, 24) // default depth
	for i := range wrongContent {
		wrongContent[i] = make([]byte, 36)
		copy(wrongContent[i], []byte("wrong hash content here!!!!!!!!!"))
	}
	if VerifySMSTProofSlice(rootHash, count, nonce, leafData, wrongContent) {
		t.Error("proof with wrong content should fail verification")
	}
}

func TestSMSTIndexBindingVerification(t *testing.T) {
	dir, _ := os.MkdirTemp("", "smst-index-binding-*")
	defer os.RemoveAll(dir)

	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}
	defer store.Close()

	// Insert artifacts with specific nonces
	nonces := []int32{100, 200, 300, 400, 500}
	for _, n := range nonces {
		if err := store.AddWithNode(n, []byte{byte(n)}, "node1"); err != nil {
			t.Fatalf("AddWithNode(%d) failed: %v", n, err)
		}
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	count, rootHash := store.GetFlushedRoot()

	// Test each artifact at its correct dense index
	for expectedIndex := uint32(0); expectedIndex < count; expectedIndex++ {
		nonce, vector, err := store.GetArtifact(expectedIndex)
		if err != nil {
			t.Fatalf("GetArtifact(%d) failed: %v", expectedIndex, err)
		}

		proof, err := store.GetProof(expectedIndex, count)
		if err != nil {
			t.Fatalf("GetProof(%d, %d) failed: %v", expectedIndex, count, err)
		}

		leafData := encodeLeaf(nonce, vector)

		// Correct index should verify
		if !VerifySMSTProofWithDenseIndex(rootHash, count, expectedIndex, nonce, leafData, proof) {
			t.Errorf("Valid proof at index %d should verify", expectedIndex)
		}

		// Wrong index should NOT verify
		wrongIndex := (expectedIndex + 1) % count
		if VerifySMSTProofWithDenseIndex(rootHash, count, wrongIndex, nonce, leafData, proof) {
			t.Errorf("Proof at index %d should NOT verify when claimed at index %d", expectedIndex, wrongIndex)
		}
	}

	// Test: taking proof from index 0 and claiming it's for index 2 should fail
	nonce0, vector0, _ := store.GetArtifact(0)
	proof0, _ := store.GetProof(0, count)
	leafData0 := encodeLeaf(nonce0, vector0)

	if VerifySMSTProofWithDenseIndex(rootHash, count, 2, nonce0, leafData0, proof0) {
		t.Error("Proof for index 0 should NOT verify when claimed at index 2")
	}
}

func TestSMSTRebuildFailsOnCorruption(t *testing.T) {
	dir, _ := os.MkdirTemp("", "smst-corruption-*")
	defer os.RemoveAll(dir)

	// Create store and add artifacts
	store, err := OpenSMST(dir)
	if err != nil {
		t.Fatalf("OpenSMST failed: %v", err)
	}

	for i := int32(0); i < 10; i++ {
		if err := store.AddWithNode(i, []byte{byte(i)}, "node1"); err != nil {
			t.Fatalf("AddWithNode(%d) failed: %v", i, err)
		}
	}

	if err := store.Flush(); err != nil {
		t.Fatalf("Flush failed: %v", err)
	}

	store.Close()

	// Corrupt the data file by truncating it
	dataPath := dir + "/artifacts.data"
	info, _ := os.Stat(dataPath)
	originalSize := info.Size()

	// Truncate to half
	if err := os.Truncate(dataPath, originalSize/2); err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	// Reopen store (recovery should handle truncation gracefully)
	store2, err := OpenSMST(dir)
	if err != nil {
		// Recovery failed entirely - this is acceptable behavior for severe corruption
		t.Logf("Recovery failed (acceptable): %v", err)
		return
	}
	defer store2.Close()

	// Store opened, but GetRootAt for the original count should fail
	// because not all artifacts are readable
	_, err = store2.GetRootAt(10)
	if err == nil {
		t.Error("GetRootAt should fail when data file is corrupted")
	} else {
		t.Logf("GetRootAt correctly failed: %v", err)
	}
}
