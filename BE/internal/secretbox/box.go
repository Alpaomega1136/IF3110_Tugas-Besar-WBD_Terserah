// Package secretbox encrypts backend secrets with purpose-bound AES-256-GCM.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
)

type Box struct{ aead cipher.AEAD }

func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, errors.New("secret key must contain 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead}, nil
}

func (b *Box) Seal(value []byte, purpose string) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return b.aead.Seal(nonce, nonce, value, []byte(purpose))
}

func (b *Box) Open(value []byte, purpose string) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(value) < n+b.aead.Overhead() {
		return nil, errors.New("invalid ciphertext")
	}
	return b.aead.Open(nil, value[:n], value[n:], []byte(purpose))
}
