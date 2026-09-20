// Package vault implements authenticated encryption for sensitive payloads
// (prompts, shared testing/sudo block, continuation prompts) using a
// daemon-owned 256-bit master key stored with 0600 permissions.
//
// Threat model: protects against accidental plaintext exposure (logs, dumps,
// DB files). It does NOT protect against an attacker controlling the same
// Unix account, who can read both the ciphertext and the key file.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const keyBytes = 32

// Vault wraps the master key.
type Vault struct {
	key [keyBytes]byte
}

// Open loads (or creates) the master key at path with 0600 permissions.
func Open(path string) (*Vault, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	v := &Vault{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if _, err := rand.Read(v.key[:]); err != nil {
			return nil, fmt.Errorf("vault: key gen: %w", err)
		}
		if err := os.WriteFile(path, v.key[:], 0o600); err != nil {
			return nil, fmt.Errorf("vault: key write: %w", err)
		}
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) != keyBytes {
		return nil, fmt.Errorf("vault: key file has wrong size %d", len(data))
	}
	copy(v.key[:], data)
	return v, nil
}

// Seal encrypts with AES-256-GCM. Returns base64(nonce||ciphertext).
func (v *Vault) Seal(plaintext string) (string, error) {
	block, err := aes.NewCipher(v.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Open decrypts a payload produced by Seal. Fails closed on tamper.
func (v *Vault) Open(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("vault: base64: %w", err)
	}
	block, err := aes.NewCipher(v.key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("vault: payload too short")
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", errors.New("vault: authentication failed (wrong key or tampered payload)")
	}
	return string(pt), nil
}
