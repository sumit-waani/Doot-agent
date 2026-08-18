// Package secretbox encrypts credentials at rest with AES-256-GCM.
//
// Secrets live in the database rather than in environment variables so that a
// rotated GitHub PAT or API key can be pasted in from a phone. The encryption
// key itself is the one credential that must come from the environment.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

var (
	ErrKeySize   = fmt.Errorf("secretbox: key must be %d bytes", KeySize)
	ErrNoKey     = errors.New("secretbox: DOOT_MASTER_KEY is not set")
	ErrCorrupt   = errors.New("secretbox: ciphertext is corrupt or was encrypted with a different key")
	ErrNonceSize = errors.New("secretbox: nonce has the wrong length")
)

// Box seals and opens secrets under a single master key.
type Box struct {
	aead cipher.AEAD
}

// New builds a Box from a raw 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secretbox: new GCM: %w", err)
	}
	return &Box{aead: aead}, nil
}

// FromBase64 builds a Box from a base64-encoded key, accepting both standard
// and URL alphabets with or without padding, since the value is usually pasted
// by hand.
func FromBase64(encoded string) (*Box, error) {
	if encoded == "" {
		return nil, ErrNoKey
	}
	key, err := decodeAny(encoded)
	if err != nil {
		return nil, fmt.Errorf("secretbox: decode key: %w", err)
	}
	return New(key)
}

// GenerateKey returns a new random key, base64-encoded for storing as an env var.
func GenerateKey() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secretbox: generate key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Seal encrypts plaintext, returning the ciphertext and the nonce it used.
// The two are stored in separate columns, matching the secrets table.
//
// name is bound as additional authenticated data, so a ciphertext copied from
// one secret's row into another's will fail to open rather than silently
// decrypt as the wrong credential.
func (b *Box) Seal(name, plaintext string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("secretbox: generate nonce: %w", err)
	}
	ciphertext = b.aead.Seal(nil, nonce, []byte(plaintext), []byte(name))
	return ciphertext, nonce, nil
}

// Open decrypts a stored secret.
func (b *Box) Open(name string, ciphertext, nonce []byte) (string, error) {
	if len(nonce) != b.aead.NonceSize() {
		return "", ErrNonceSize
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, []byte(name))
	if err != nil {
		return "", ErrCorrupt
	}
	return string(plaintext), nil
}

func decodeAny(s string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	var lastErr error
	for _, enc := range encodings {
		out, err := enc.DecodeString(s)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
