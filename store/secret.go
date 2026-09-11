package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeyBytes is the length of a secret key, for AES-256.
const KeyBytes = 32

// sealPrefix marks an encrypted value and identifies the scheme, so that a value
// written by another scheme, or not encrypted at all, is recognised rather than
// decrypted into nonsense.
const sealPrefix = "rbk1."

// ParseKey reads a secret key from its hex representation.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("store: secret key is empty")
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("store: secret key is not hexadecimal: %w", err)
	}
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("store: secret key is %d bytes, want %d (%d hex characters)",
			len(key), KeyBytes, KeyBytes*2)
	}
	return key, nil
}

// NewKey returns a new random secret key as hex.
func NewKey() (string, error) {
	key := make([]byte, KeyBytes)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("store: generate secret key: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// seal encrypts a value with AES-GCM.
//
// The label is authenticated but not stored, so a ciphertext cannot be moved to
// another row or column: a refresh token copied into the access token field
// fails to open.
func seal(key []byte, label, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if len(key) == 0 {
		return plaintext, nil
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("store: generate nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), []byte(label))
	return sealPrefix + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// unseal decrypts a value written by seal.
func unseal(key []byte, label, stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	encrypted := strings.HasPrefix(stored, sealPrefix)

	switch {
	case len(key) == 0 && !encrypted:
		return stored, nil
	case len(key) == 0 && encrypted:
		return "", errors.New("store: stored credential is encrypted but no secret key is configured")
	case !encrypted:
		return "", errors.New("store: stored credential is not encrypted; re-authorize to replace it")
	}

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(stored, sealPrefix))
	if err != nil {
		return "", fmt.Errorf("store: stored credential is malformed: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("store: stored credential is truncated")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte(label))
	if err != nil {
		return "", fmt.Errorf("store: cannot decrypt credential, the secret key may have changed: %w", err)
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("store: secret key is unusable: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("store: cannot use AES-GCM: %w", err)
	}
	return gcm, nil
}

// label names the field a ciphertext belongs to.
func label(server, field string) string { return server + "/" + field }
