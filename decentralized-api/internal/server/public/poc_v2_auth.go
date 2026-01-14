package public

import (
	"fmt"
	"os"

	"decentralized-api/utils"

	"github.com/productscience/inference/x/inference/calculations"
)

// Temporary PoC v2 start gate:
// Only allow requests signed by a single public key.
//
// Prefer setting via env for ops convenience, but keep the mechanism isolated so it can be removed later.
const (
	pocV2StartAllowedPubKeyEnv          = "POC_V2_START_PUBKEY"
	pocV2StartAllowedPubKeyBase64       = "A5m9wCLDGW4Mnye1YdmeyrWemnnxkrb2jPmVqUk66zMp"
	pocV2StartSignatureTimestamp  int64 = 0
)

func getPoCV2StartAllowedPubKeyBase64() (string, error) {
	if v := os.Getenv(pocV2StartAllowedPubKeyEnv); v != "" {
		return v, nil
	}
	if pocV2StartAllowedPubKeyBase64 != "" {
		return pocV2StartAllowedPubKeyBase64, nil
	}
	return "", fmt.Errorf("missing allowed pubkey: set %s or set pocV2StartAllowedPubKeyBase64", pocV2StartAllowedPubKeyEnv)
}

func validatePoCV2StartSignature(signature string, payloadHash string) error {
	pubKey, err := getPoCV2StartAllowedPubKeyBase64()
	if err != nil {
		return err
	}

	components := calculations.SignatureComponents{
		Payload:         payloadHash,
		Timestamp:       pocV2StartSignatureTimestamp,
		TransferAddress: "",
		ExecutorAddress: "",
	}

	return calculations.ValidateSignature(components, calculations.Developer, pubKey, signature)
}

func pocV2StartPayloadHashFromBody(canonicalJSON string) string {
	return utils.GenerateSHA256Hash(canonicalJSON)
}
