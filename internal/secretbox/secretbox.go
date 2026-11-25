// Package secretbox encrypts credentials at rest with AES-256-GCM.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

const (
	keyBytes   = 32
	keyIDChars = 16
)

// Box seals and opens values with one key; KeyID tells rows apart after a key change.
type Box struct {
	gcm   cipher.AEAD
	keyID string
}

// New builds a Box from a standard-base64 encoded 32 byte key. Errors never contain the key.
func New(keyBase64 string) (*Box, error) {
	key, err := base64.StdEncoding.DecodeString(keyBase64)
	if err != nil {
		return nil, errors.New("secret key is not valid base64")
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("secret key must be %d bytes, got %d", keyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errors.New("secret key rejected by cipher")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errors.New("cannot initialise GCM")
	}
	sum := sha256.Sum256(key)
	return &Box{gcm: gcm, keyID: hex.EncodeToString(sum[:])[:keyIDChars]}, nil
}

// KeyID identifies the key without revealing it.
func (b *Box) KeyID() string { return b.keyID }

// Seal returns nonce || ciphertext.
func (b *Box) Seal(plain []byte) ([]byte, error) {
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return b.gcm.Seal(nonce, nonce, plain, nil), nil
}

// Open decrypts a value sealed with the key named keyID; any mismatch or tampering is an error.
func (b *Box) Open(keyID string, sealed []byte) ([]byte, error) {
	if keyID != b.keyID {
		return nil, errors.New("credential was sealed with a different key")
	}
	n := b.gcm.NonceSize()
	if len(sealed) < n+b.gcm.Overhead() {
		return nil, errors.New("sealed credential too short")
	}
	plain, err := b.gcm.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, errors.New("credential cannot be decrypted")
	}
	return plain, nil
}
