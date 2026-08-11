// Package secure wraps the small amount of cryptography henri needs.
//
// One 32-byte group secret lives in the config file. Two independent keys are
// derived from it with HKDF-SHA256 so a discovery beacon can never be replayed
// as a clipboard payload (or vice versa):
//
//	sync      -> AES-256-GCM over TCP, carries clipboard contents
//	discovery -> AES-256-GCM over UDP, carries presence beacons
//
// Authentication is implicit: holding the group secret is what makes a device a
// member, and GCM's tag rejects anything sealed under a different key. The
// group ID is bound in as additional data so ciphertexts cannot be replayed
// into a different group that happens to share a secret.
package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// Key labels passed to HKDF.
const (
	InfoSync      = "henri sync v1"
	InfoDiscovery = "henri discovery v1"
)

// ErrBadCiphertext is returned when a frame fails to authenticate. It never
// says why: a peer that cannot decrypt is simply not a member of the group.
var ErrBadCiphertext = errors.New("henri: message failed authentication")

// Box seals and opens messages for one purpose (sync or discovery).
type Box struct {
	aead    cipher.AEAD
	groupAD []byte
}

// NewBox derives a purpose-specific key from the group master secret.
func NewBox(master []byte, groupID, info string) (*Box, error) {
	if len(master) != 32 {
		return nil, fmt.Errorf("henri: master key must be 32 bytes, got %d", len(master))
	}
	key, err := hkdf.Key(sha256.New, master, []byte(groupID), info, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead, groupAD: []byte(groupID)}, nil
}

// Seal encrypts plaintext, returning nonce||ciphertext||tag.
func (b *Box) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, b.groupAD), nil
}

// Open reverses Seal.
func (b *Box) Open(sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < n+b.aead.Overhead() {
		return nil, ErrBadCiphertext
	}
	out, err := b.aead.Open(nil, sealed[:n], sealed[n:], b.groupAD)
	if err != nil {
		return nil, ErrBadCiphertext
	}
	return out, nil
}

// Hash is the content fingerprint used to decide whether a clipboard payload is
// new. It is also what stops a copy from echoing back and forth between peers.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
