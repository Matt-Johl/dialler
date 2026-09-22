// Package secrets encrypts the few credentials this server must be able to
// replay rather than merely check.
//
// Almost everything the server stores is a one-way hash — a device token's
// SHA-256, a SIP Digest H(A1) — because it only ever has to *verify*. A PBX
// line credential is different: the server has to present it to the exchange
// on every registration, so it must be recoverable, and a plain password
// would otherwise sit in devices.json.
//
// What this buys, stated plainly: a copy of devices.json — a backup, an
// export, a support bundle — is useless on its own. It is NOT protection
// from anyone who can read the whole data directory, since the key lives
// there too. That is the honest limit of a key file beside its data, and the
// alternative (an operator-supplied passphrase at every start) trades a
// server that survives a reboot unattended for it.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// keyPrefix versions the sealed form so a future key rotation can tell the
// two apart rather than guessing.
const keyPrefix = "v1:"

// Box seals and opens secrets with one key.
type Box struct{ aead cipher.AEAD }

// OpenKey loads the 32-byte key at path, minting one on first use. The file
// is written 0600 in a directory created 0700.
func OpenKey(path string) (*Box, error) {
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("secrets: write %s: %w", path, err)
		}
		return New(key)
	case err != nil:
		return nil, fmt.Errorf("secrets: read %s: %w", path, err)
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("secrets: %s is not a hex key: %w", path, err)
	}
	return New(key)
}

// New builds a box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts s. The result is safe to store and carries its own nonce.
// The label is bound into the ciphertext as additional data, so a sealed
// value cannot be lifted from one device's record into another's.
func (b *Box) Seal(label, s string) (string, error) {
	if b == nil {
		return "", errors.New("secrets: no key configured")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(s), []byte(label))
	return keyPrefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open decrypts what Seal produced for the same label.
func (b *Box) Open(label, sealed string) (string, error) {
	if b == nil {
		return "", errors.New("secrets: no key configured")
	}
	if !strings.HasPrefix(sealed, keyPrefix) {
		return "", errors.New("secrets: unrecognised sealed value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, keyPrefix))
	if err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("secrets: sealed value too short")
	}
	out, err := b.aead.Open(nil, raw[:n], raw[n:], []byte(label))
	if err != nil {
		// Deliberately not "wrong key" vs "tampered": both mean the same
		// thing to an operator, and saying which helps nobody else.
		return "", errors.New("secrets: cannot decrypt (wrong key, or the value was altered)")
	}
	return string(out), nil
}
