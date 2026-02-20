package artifacts

import "encoding/binary"

// Note: Simple proof verification without counts is NOT possible for SMST.
// SMST proofs MUST include sibling counts to verify correctly because the
// internal node hash includes the count. Use VerifySMSTProofWithCounts or
// VerifySMSTProofSlice instead.

func smstNoncePath(nonce int32, depth int) []bool {
	path := make([]bool, depth)
	n := uint32(nonce)
	for i := 0; i < depth; i++ {
		bit := (n >> (depth - 1 - i)) & 1
		path[i] = bit == 1
	}
	return path
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// VerifySMSTProofWithCounts verifies an SMST proof with explicit sibling counts.
// Proof elements are ordered from root (index 0) to leaf (index depth-1).
// Verification reconstructs the root by starting at the leaf and going up.
func VerifySMSTProofWithCounts(rootHash []byte, count uint32, nonce int32, leafData []byte, proof []SMSTProofElement) bool {
	return VerifySMSTProofWithCountsDebug(rootHash, count, nonce, leafData, proof, false)
}

// VerifySMSTProofWithCountsDebug is like VerifySMSTProofWithCounts but with optional debug output.
func VerifySMSTProofWithCountsDebug(rootHash []byte, count uint32, nonce int32, leafData []byte, proof []SMSTProofElement, debug bool) bool {
	if len(proof) == 0 {
		return false
	}

	depth := len(proof)
	leafHash := smstHashLeaf(leafData)
	path := smstNoncePath(nonce, depth)

	currentHash := leafHash
	currentCount := uint32(1)

	for i := depth - 1; i >= 0; i-- {
		elem := proof[i]
		goRight := path[i]

		var leftHash, rightHash []byte
		var leftCount, rightCount uint32

		if goRight {
			leftHash = elem.SiblingHash
			rightHash = currentHash
			leftCount = elem.SiblingCount
			rightCount = currentCount
		} else {
			leftHash = currentHash
			rightHash = elem.SiblingHash
			leftCount = currentCount
			rightCount = elem.SiblingCount
		}

		currentCount = leftCount + rightCount
		currentHash = smstHashNode(leftHash, rightHash, currentCount)
	}

	if currentCount != count {
		return false
	}

	return bytesEqual(currentHash, rootHash)
}

// SMSTProofElement contains a sibling hash and its subtree count.
type SMSTProofElement struct {
	SiblingHash  []byte
	SiblingCount uint32
}

// EncodeSMSTProof encodes a proof with counts to bytes.
// Format: [depth(1)][hash(32)][count(4)]...
func EncodeSMSTProof(proof []SMSTProofElement) []byte {
	if len(proof) == 0 {
		return nil
	}

	buf := make([]byte, 1+len(proof)*36)
	buf[0] = byte(len(proof))

	offset := 1
	for _, elem := range proof {
		copy(buf[offset:offset+32], elem.SiblingHash)
		binary.LittleEndian.PutUint32(buf[offset+32:offset+36], elem.SiblingCount)
		offset += 36
	}

	return buf
}

// DecodeSMSTProof decodes a proof with counts from bytes.
func DecodeSMSTProof(data []byte) ([]SMSTProofElement, error) {
	if len(data) < 1 {
		return nil, ErrLeafIndexOutOfRange
	}

	depth := int(data[0])
	if len(data) != 1+depth*36 {
		return nil, ErrLeafIndexOutOfRange
	}

	proof := make([]SMSTProofElement, depth)
	offset := 1
	for i := 0; i < depth; i++ {
		proof[i].SiblingHash = make([]byte, 32)
		copy(proof[i].SiblingHash, data[offset:offset+32])
		proof[i].SiblingCount = binary.LittleEndian.Uint32(data[offset+32 : offset+36])
		offset += 36
	}

	return proof, nil
}

// DecodeProofElements decodes proof from slice format ([][]byte where each is 36 bytes).
// Returns nil if any element is malformed (not exactly 36 bytes).
func DecodeProofElements(proof [][]byte) []SMSTProofElement {
	elements := make([]SMSTProofElement, len(proof))
	for i, data := range proof {
		if len(data) != 36 {
			return nil
		}
		elements[i].SiblingHash = data[:32]
		elements[i].SiblingCount = binary.LittleEndian.Uint32(data[32:36])
	}
	return elements
}

// VerifySMSTProofSlice verifies an SMST proof in slice format ([][]byte).
// Each proof element is 36 bytes: 32 bytes hash + 4 bytes count (LE).
// This matches the format returned by SMSTArtifactStore.GetProof().
func VerifySMSTProofSlice(rootHash []byte, count uint32, nonce int32, leafData []byte, proof [][]byte) bool {
	if len(proof) == 0 {
		return false
	}

	elements := DecodeProofElements(proof)
	if elements == nil {
		return false
	}
	return VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements)
}

// VerifySMSTProofWithDenseIndex verifies an SMST proof and checks that the nonce
// is at the claimed dense index position. The sibling counts in the proof are
// committed by the root hash, making the index binding cryptographically sound.
func VerifySMSTProofWithDenseIndex(rootHash []byte, count uint32, denseIndex uint32, nonce int32, leafData []byte, proof [][]byte) bool {
	if len(proof) == 0 || denseIndex >= count {
		return false
	}

	elements := DecodeProofElements(proof)
	if elements == nil {
		return false
	}

	if !VerifySMSTProofWithCounts(rootHash, count, nonce, leafData, elements) {
		return false
	}

	depth := len(elements)
	path := smstNoncePath(nonce, depth)

	computedIndex := uint32(0)
	for i := 0; i < depth; i++ {
		if path[i] {
			computedIndex += elements[i].SiblingCount
		}
	}

	return computedIndex == denseIndex
}
