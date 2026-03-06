package signing

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// Secp256k1Signer signs messages using a secp256k1 private key.
type Secp256k1Signer struct {
	key     *ecdsa.PrivateKey
	address string
}

func NewSecp256k1Signer(key *ecdsa.PrivateKey) *Secp256k1Signer {
	addr := crypto.PubkeyToAddress(key.PublicKey)
	return &Secp256k1Signer{
		key:     key,
		address: addr.Hex(),
	}
}

func (s *Secp256k1Signer) Sign(message []byte) ([]byte, error) {
	hash := sha256.Sum256(message)
	return crypto.Sign(hash[:], s.key)
}

func (s *Secp256k1Signer) Address() string {
	return s.address
}

// Secp256k1Verifier recovers addresses from secp256k1 signatures.
type Secp256k1Verifier struct{}

func NewSecp256k1Verifier() *Secp256k1Verifier {
	return &Secp256k1Verifier{}
}

func (v *Secp256k1Verifier) RecoverAddress(message []byte, sig []byte) (string, error) {
	if len(sig) != 65 {
		return "", fmt.Errorf("invalid signature length: %d", len(sig))
	}
	hash := sha256.Sum256(message)
	pubkey, err := crypto.Ecrecover(hash[:], sig)
	if err != nil {
		return "", fmt.Errorf("ecrecover failed: %w", err)
	}
	pubkeyECDSA, err := crypto.UnmarshalPubkey(pubkey)
	if err != nil {
		return "", fmt.Errorf("unmarshal pubkey failed: %w", err)
	}
	addr := crypto.PubkeyToAddress(*pubkeyECDSA)
	return addr.Hex(), nil
}

func (s *Secp256k1Signer) PublicKeyBytes() []byte {
	return crypto.FromECDSAPub(&s.key.PublicKey)
}

func GenerateKey() (*Secp256k1Signer, error) {
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	return NewSecp256k1Signer(key), nil
}

// SignerFromHex creates a signer from a hex-encoded private key.
func SignerFromHex(hexKey string) (*Secp256k1Signer, error) {
	key, err := crypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, fmt.Errorf("parse hex key: %w", err)
	}
	return NewSecp256k1Signer(key), nil
}

func AddressFromPubKey(pubkey []byte) string {
	return common.BytesToAddress(crypto.Keccak256(pubkey[1:])[12:]).Hex()
}
