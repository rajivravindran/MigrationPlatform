package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// DecryptSecret decrypts AES-256-GCM ciphertext produced by the Rust API
// MasterKey helper (apps/api/src/security/secrets.rs). MASTER_KEY is either
// standard base64 of 32 raw bytes, or a raw string >= 32 chars (SHA-256 hashed).
func DecryptSecret(masterKeyB64 string, ciphertext, nonce []byte) ([]byte, error) {
	key, err := decodeMasterKey(masterKeyB64)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid nonce size %d (want %d)", len(nonce), gcm.NonceSize())
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("aes-gcm decrypt: %w", err)
	}
	return plain, nil
}

func decodeMasterKey(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("MASTER_KEY is empty")
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(value) < 32 {
		return nil, errors.New("MASTER_KEY must be base64 of 32 bytes or a raw string >= 32 chars")
	}
	sum := sha256.Sum256([]byte(value))
	return sum[:], nil
}
