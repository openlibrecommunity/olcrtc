// Package crypto provides cryptographic functions.
package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	ErrInvalidKeySize     = errors.New("invalid key size")     //nolint:revive
	ErrCiphertextTooShort = errors.New("ciphertext too short") //nolint:revive
)

type Cipher struct { //nolint:revive
	aead cipher.AEAD
}

func NewCipher(keyStr string) (*Cipher, error) { //nolint:revive
	key := []byte(keyStr)
	if len(key) != chacha20poly1305.KeySize {
		return nil, ErrInvalidKeySize
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create aead: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) { //nolint:revive
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to read nonce: %w", err)
	}

	ciphertext := c.aead.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) { //nolint:revive
	if len(ciphertext) < c.aead.NonceSize() {
		return nil, ErrCiphertextTooShort
	}

	nonce := ciphertext[:c.aead.NonceSize()]
	encrypted := ciphertext[c.aead.NonceSize():]

	res, err := c.aead.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	return res, nil
}
