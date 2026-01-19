package pocv2

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"decentralized-api/cosmosclient"
	"decentralized-api/pocartifacts"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

func TestBuildProofSignPayload(t *testing.T) {
	pocHeight := int64(1000)
	rootHash := make([]byte, 32)
	for i := range rootHash {
		rootHash[i] = byte(i)
	}
	count := uint32(100)
	leafIndices := []uint32{0, 5, 10}
	timestamp := int64(1234567890)
	validatorAddress := "validator123"
	signerAddress := "signer456"

	payload := buildProofSignPayload(pocHeight, rootHash, count, leafIndices, timestamp, validatorAddress, signerAddress)

	// Payload should be hex-encoded SHA256 hash
	assert.Equal(t, 64, len(payload), "payload should be 64 hex chars")

	// Verify payload is deterministic
	payload2 := buildProofSignPayload(pocHeight, rootHash, count, leafIndices, timestamp, validatorAddress, signerAddress)
	assert.True(t, bytes.Equal(payload, payload2), "payload should be deterministic")

	// Verify different inputs produce different payloads
	payload3 := buildProofSignPayload(pocHeight+1, rootHash, count, leafIndices, timestamp, validatorAddress, signerAddress)
	assert.False(t, bytes.Equal(payload, payload3), "different height should produce different payload")
}

func TestBuildLeafData(t *testing.T) {
	nonce := int32(42)
	vector := []byte{1, 2, 3, 4, 5}

	leafData := buildLeafData(nonce, vector)

	// Should be 4 bytes (nonce LE32) + len(vector)
	assert.Equal(t, 4+len(vector), len(leafData))

	// Verify nonce encoding
	extractedNonce := int32(binary.LittleEndian.Uint32(leafData[:4]))
	assert.Equal(t, nonce, extractedNonce)

	// Verify vector
	assert.True(t, bytes.Equal(vector, leafData[4:]))
}

func TestCheckDuplicateNonces(t *testing.T) {
	tests := []struct {
		name        string
		artifacts   []VerifiedArtifact
		expectError error
	}{
		{
			name:        "empty",
			artifacts:   []VerifiedArtifact{},
			expectError: nil,
		},
		{
			name: "no duplicates",
			artifacts: []VerifiedArtifact{
				{LeafIndex: 0, Nonce: 1, VectorB64: "YQ=="},
				{LeafIndex: 1, Nonce: 2, VectorB64: "Yg=="},
				{LeafIndex: 2, Nonce: 3, VectorB64: "Yw=="},
			},
			expectError: nil,
		},
		{
			name: "has duplicates",
			artifacts: []VerifiedArtifact{
				{LeafIndex: 0, Nonce: 1, VectorB64: "YQ=="},
				{LeafIndex: 1, Nonce: 2, VectorB64: "Yg=="},
				{LeafIndex: 2, Nonce: 1, VectorB64: "Yw=="}, // duplicate nonce
			},
			expectError: ErrDuplicateNonces,
		},
		{
			name: "consecutive duplicates",
			artifacts: []VerifiedArtifact{
				{LeafIndex: 0, Nonce: 5, VectorB64: "YQ=="},
				{LeafIndex: 1, Nonce: 5, VectorB64: "Yg=="}, // duplicate
			},
			expectError: ErrDuplicateNonces,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckDuplicateNonces(tt.artifacts)
			if tt.expectError != nil {
				assert.ErrorIs(t, err, tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestFetchAndVerifyProofs_Success(t *testing.T) {
	// Create a simple artifact store to generate valid proofs
	dir := t.TempDir()
	store, err := pocartifacts.Open(dir)
	assert.NoError(t, err)
	defer store.Close()

	// Add some artifacts
	for i := int32(0); i < 10; i++ {
		err := store.Add(i, []byte{byte(i)})
		assert.NoError(t, err)
	}
	err = store.Flush()
	assert.NoError(t, err)

	rootHash := store.GetRoot()
	count := uint32(10)

	// Create test server that returns valid proofs
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/poc/proofs", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)

		// Parse request
		var req map[string]interface{}
		json.NewDecoder(r.Body).Decode(&req)

		leafIndices := req["leaf_indices"].([]interface{})

		// Build response with valid proofs
		proofs := make([]ProofItem, 0, len(leafIndices))
		for _, idxFloat := range leafIndices {
			idx := uint32(idxFloat.(float64))
			nonce := int32(idx)
			vector := []byte{byte(idx)}

			proof, err := store.GetProof(idx, count)
			assert.NoError(t, err)

			proofB64 := make([]string, len(proof))
			for i, h := range proof {
				proofB64[i] = base64.StdEncoding.EncodeToString(h)
			}

			proofs = append(proofs, ProofItem{
				LeafIndex:   idx,
				NonceValue:  nonce,
				VectorBytes: base64.StdEncoding.EncodeToString(vector),
				Proof:       proofB64,
			})
		}

		json.NewEncoder(w).Encode(ProofResponse{Proofs: proofs})
	}))
	defer server.Close()

	// Create mock recorder
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("GetAccountAddress").Return("validator123")
	mockRecorder.On("GetSignerAddress").Return("signer456")
	mockRecorder.On("SignBytes", mock.Anything).Return([]byte("signature"), nil)

	// Create proof client
	client := NewProofClient(mockRecorder, DefaultProofClientConfig())

	// Test fetch and verify
	verified, err := client.FetchAndVerifyProofs(
		context.Background(),
		server.URL,
		ProofRequest{
			PocStageStartBlockHeight: 1000,
			RootHash:                 rootHash,
			Count:                    count,
			LeafIndices:              []uint32{0, 3, 7},
			ParticipantAddress:       "participant123",
		},
	)

	assert.NoError(t, err)
	assert.Len(t, verified, 3)
	assert.Equal(t, uint32(0), verified[0].LeafIndex)
	assert.Equal(t, int32(0), verified[0].Nonce)
}

func TestFetchAndVerifyProofs_InvalidProof(t *testing.T) {
	// Create test server that returns invalid proofs
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return proof with wrong data (proof won't verify)
		proofs := []ProofItem{
			{
				LeafIndex:   0,
				NonceValue:  42,
				VectorBytes: base64.StdEncoding.EncodeToString([]byte{1, 2, 3}),
				Proof: []string{
					base64.StdEncoding.EncodeToString(make([]byte, 32)), // invalid proof
				},
			},
		}
		json.NewEncoder(w).Encode(ProofResponse{Proofs: proofs})
	}))
	defer server.Close()

	// Create mock recorder
	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("GetAccountAddress").Return("validator123")
	mockRecorder.On("GetSignerAddress").Return("signer456")
	mockRecorder.On("SignBytes", mock.Anything).Return([]byte("signature"), nil)

	client := NewProofClient(mockRecorder, DefaultProofClientConfig())

	// Create a fake root hash
	rootHash := make([]byte, 32)
	sha256.Sum256([]byte("test"))

	_, err := client.FetchAndVerifyProofs(
		context.Background(),
		server.URL,
		ProofRequest{
			PocStageStartBlockHeight: 1000,
			RootHash:                 rootHash,
			Count:                    10,
			LeafIndices:              []uint32{0},
			ParticipantAddress:       "participant123",
		},
	)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "proof verification failed")
}

func TestFetchAndVerifyProofs_HTTPError(t *testing.T) {
	// Create test server that returns error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer server.Close()

	mockRecorder := &cosmosclient.MockCosmosMessageClient{}
	mockRecorder.On("GetAccountAddress").Return("validator123")
	mockRecorder.On("GetSignerAddress").Return("signer456")
	mockRecorder.On("SignBytes", mock.Anything).Return([]byte("signature"), nil)

	client := NewProofClient(mockRecorder, DefaultProofClientConfig())

	_, err := client.FetchAndVerifyProofs(
		context.Background(),
		server.URL,
		ProofRequest{
			PocStageStartBlockHeight: 1000,
			RootHash:                 make([]byte, 32),
			Count:                    10,
			LeafIndices:              []uint32{0},
			ParticipantAddress:       "participant123",
		},
	)

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestProofSignPayloadFormat(t *testing.T) {
	// Test that the payload format matches what the server expects
	pocHeight := int64(12345)
	rootHash := bytes.Repeat([]byte{0xAB}, 32)
	count := uint32(100)
	leafIndices := []uint32{1, 2, 3}
	timestamp := int64(9999999999)
	validatorAddr := "cosmos1abc"
	signerAddr := "cosmos1xyz"

	payload := buildProofSignPayload(pocHeight, rootHash, count, leafIndices, timestamp, validatorAddr, signerAddr)

	// Verify it's valid hex
	decoded, err := hex.DecodeString(string(payload))
	assert.NoError(t, err)
	assert.Len(t, decoded, 32, "should be SHA256 hash (32 bytes)")
}
