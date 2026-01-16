package public

import (
	"bytes"
	"crypto/sha256"
	"decentralized-api/logging"
	"decentralized-api/pocartifacts"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	"github.com/labstack/echo/v4"
	"github.com/productscience/inference/x/inference/types"
)

const (
	maxLeafIndicesPerRequest = 500
	pocProofsMsgTypeUrl      = "/inference.inference.MsgStartInference"
	timestampWindowNanos     = 5 * 60 * 1_000_000_000 // 5 minutes in nanoseconds
)

// StringInt64 unmarshals from both JSON number and string
type StringInt64 int64

func (s *StringInt64) UnmarshalJSON(data []byte) error {
	var num int64
	if err := json.Unmarshal(data, &num); err == nil {
		*s = StringInt64(num)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	num, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return err
	}
	*s = StringInt64(num)
	return nil
}

// StringUint32 unmarshals from both JSON number and string
type StringUint32 uint32

func (s *StringUint32) UnmarshalJSON(data []byte) error {
	var num uint32
	if err := json.Unmarshal(data, &num); err == nil {
		*s = StringUint32(num)
		return nil
	}
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	num64, err := strconv.ParseUint(str, 10, 32)
	if err != nil {
		return err
	}
	*s = StringUint32(num64)
	return nil
}

// PocProofsRequest is the request body for POST /v1/poc/proofs
// Uses StringInt64/StringUint32 to accept both number and string JSON encoding
type PocProofsRequest struct {
	PocStageStartBlockHeight StringInt64    `json:"poc_stage_start_block_height"`
	RootHash                 string         `json:"root_hash"`    // base64-encoded 32 bytes
	Count                    StringUint32   `json:"count"`        // snapshot leaf count
	LeafIndices              []StringUint32 `json:"leaf_indices"` // 0-based indices

	ValidatorAddress       string      `json:"validator_address"`        // validator's cold key (for authz lookup)
	ValidatorSignerAddress string      `json:"validator_signer_address"` // actual signer (cold or warm key)
	Timestamp              StringInt64 `json:"timestamp"`                // unix nanoseconds
	Signature              string      `json:"signature"`                // base64-encoded signature
}

// PocProofItem is a single proof in the response
type PocProofItem struct {
	LeafIndex   uint32   `json:"leaf_index"`
	NonceValue  int32    `json:"nonce_value"`
	VectorBytes string   `json:"vector_bytes"` // base64-encoded
	Proof       []string `json:"proof"`        // base64-encoded hashes
}

// PocProofsResponse is the response body for POST /v1/poc/proofs
type PocProofsResponse struct {
	Proofs []PocProofItem `json:"proofs"`
}

// postPocProofs handles POST /v1/poc/proofs
func (s *Server) postPocProofs(ctx echo.Context) error {
	if s.artifactStore == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "artifact store not configured")
	}

	var req PocProofsRequest
	if err := ctx.Bind(&req); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid request body")
	}

	// Validate required fields
	if req.RootHash == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "root_hash required")
	}
	if req.ValidatorAddress == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "validator_address required")
	}
	if req.ValidatorSignerAddress == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "validator_signer_address required")
	}
	if req.Signature == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "signature required")
	}
	if len(req.LeafIndices) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "leaf_indices required")
	}
	if len(req.LeafIndices) > maxLeafIndicesPerRequest {
		return echo.NewHTTPError(http.StatusBadRequest, "too many leaf_indices (max 500)")
	}

	// Decode root_hash
	rootHash, err := base64.StdEncoding.DecodeString(req.RootHash)
	if err != nil || len(rootHash) != 32 {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid root_hash (must be 32 bytes base64)")
	}

	// Validate timestamp is within acceptable window (+/-5 minutes)
	nowNanos := time.Now().UnixNano()
	reqTimestamp := int64(req.Timestamp)
	if reqTimestamp < nowNanos-timestampWindowNanos || reqTimestamp > nowNanos+timestampWindowNanos {
		logging.Warn("PoC proofs request timestamp out of range", types.Validation,
			"timestamp", reqTimestamp, "now", nowNanos)
		return echo.NewHTTPError(http.StatusBadRequest, "timestamp out of acceptable window")
	}

	// Get pubkeys for validator (via authz cache)
	// validator_address = cold key for authz lookup
	// validator_signer_address = actual signer (must be in pubkeys list)
	pubkeys, err := s.authzCache.GetPubKeys(ctx.Request().Context(), req.ValidatorAddress, pocProofsMsgTypeUrl)
	if err != nil {
		logging.Error("Failed to get validator pubkeys", types.Validation,
			"validatorAddress", req.ValidatorAddress, "error", err)
		return echo.NewHTTPError(http.StatusUnauthorized, "validator not found")
	}

	// Verify signature
	// TODO: accept only request from validator with weight > 0
	// TODO: use ValidatorSignerAddress to verify signature, not all warm keys
	if err := verifyPocProofsSignature(&req, rootHash, pubkeys); err != nil {
		logging.Warn("Invalid PoC proofs signature", types.Validation,
			"validatorAddress", req.ValidatorAddress,
			"validatorSignerAddress", req.ValidatorSignerAddress, "error", err)
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid signature")
	}

	// Get epoch-specific artifact store
	epochStore, err := s.artifactStore.GetStore(int64(req.PocStageStartBlockHeight))
	if err != nil {
		logging.Warn("Epoch store not found", types.Validation,
			"pocStageStartBlockHeight", req.PocStageStartBlockHeight, "error", err)
		return echo.NewHTTPError(http.StatusBadRequest, "epoch not found (may be pruned or not yet created)")
	}

	// Generate proofs
	proofs := make([]PocProofItem, 0, len(req.LeafIndices))
	for _, leafIndex := range req.LeafIndices {
		leafIdx := uint32(leafIndex)
		nonce, vector, err := epochStore.GetArtifact(leafIdx)
		if err != nil {
			if err == pocartifacts.ErrLeafIndexOutOfRange {
				return echo.NewHTTPError(http.StatusBadRequest, "leaf_index out of range")
			}
			logging.Error("Failed to get artifact", types.Validation,
				"leafIndex", leafIdx, "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get artifact")
		}

		proof, err := epochStore.GetProof(leafIdx, uint32(req.Count))
		if err != nil {
			logging.Error("Failed to get proof", types.Validation,
				"leafIndex", leafIdx, "count", req.Count, "error", err)
			return echo.NewHTTPError(http.StatusInternalServerError, "failed to get proof")
		}

		// Encode proof hashes as base64
		proofStrings := make([]string, len(proof))
		for i, hash := range proof {
			proofStrings[i] = base64.StdEncoding.EncodeToString(hash)
		}

		proofs = append(proofs, PocProofItem{
			LeafIndex:   leafIdx,
			NonceValue:  nonce,
			VectorBytes: base64.StdEncoding.EncodeToString(vector),
			Proof:       proofStrings,
		})
	}

	logging.Info("Serving PoC proofs", types.Validation,
		"validatorAddress", req.ValidatorAddress, "count", len(proofs))

	return ctx.JSON(http.StatusOK, PocProofsResponse{Proofs: proofs})
}

// buildPocProofsSignPayload builds the binary payload for signature verification.
// Format: hex(SHA256(poc_stage_start_block_height(LE64) || root_hash(32) || count(LE32) ||
//
//	leaf_indices(LE32 each) || timestamp(LE64) || validator_address || validator_signer_address))
//
// Returns the hex-encoded hash as bytes because Kotlin's signPayload takes a hex string.
func buildPocProofsSignPayload(req *PocProofsRequest, rootHash []byte) []byte {
	buf := new(bytes.Buffer)

	binary.Write(buf, binary.LittleEndian, int64(req.PocStageStartBlockHeight))
	buf.Write(rootHash)
	binary.Write(buf, binary.LittleEndian, uint32(req.Count))
	for _, idx := range req.LeafIndices {
		binary.Write(buf, binary.LittleEndian, uint32(idx))
	}
	binary.Write(buf, binary.LittleEndian, int64(req.Timestamp))
	buf.WriteString(req.ValidatorAddress)
	buf.WriteString(req.ValidatorSignerAddress)

	hash := sha256.Sum256(buf.Bytes())
	// Return hex-encoded string as bytes (what Kotlin signs)
	return []byte(hex.EncodeToString(hash[:]))
}

// verifyPocProofsSignature verifies the signature against any of the provided pubkeys
func verifyPocProofsSignature(req *PocProofsRequest, rootHash []byte, pubkeys []string) error {
	payload := buildPocProofsSignPayload(req, rootHash)

	signatureBytes, err := base64.StdEncoding.DecodeString(req.Signature)
	if err != nil {
		return err
	}

	for _, pubkeyStr := range pubkeys {
		pubkeyBytes, err := base64.StdEncoding.DecodeString(pubkeyStr)
		if err != nil {
			continue
		}

		pubkey := secp256k1.PubKey{Key: pubkeyBytes}
		if pubkey.VerifySignature(payload, signatureBytes) {
			return nil
		}
	}

	return echo.NewHTTPError(http.StatusUnauthorized, "signature verification failed")
}
