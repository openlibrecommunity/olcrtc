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
	ErrInvalidKeySize     = errors.New("invalid key size")
	ErrCiphertextTooShort = errors.New("ciphertext too short")
)

// Cipher provides AEAD encryption and decryption using ChaCha20-Poly1305.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher creates a new Cipher instance with the given 32-byte key.
func NewCipher(keyStr string) (*Cipher, error) {
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

// Encrypt encrypts plaintext and prepends a random nonce.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	// Seal appends the ciphertext to the nonce
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt decrypts ciphertext that has a nonce prepended.
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, ErrCiphertextTooShort
	}

	nonce := ciphertext[:nonceSize]
	encrypted := ciphertext[nonceSize:]

	res, err := c.aead.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	return res, nil
}
