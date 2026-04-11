//nolint:revive
package crypto

import (
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/openlibrecommunity/olcrtc/internal/errors"
)

//nolint:revive
type Cipher struct {
	aead cipher.AEAD
}

//nolint:revive
func NewCipher(keyStr string) (*Cipher, error) {
	key := []byte(keyStr)
	if len(key) != chacha20poly1305.KeySize {
		return nil, fmt.Errorf("invalid key size: %w", errors.ErrInvalidKeySize)
	}

	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	return &Cipher{aead: aead}, nil
}

//nolint:revive
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("failed to generate nonce: %w", err)
	}

	ciphertext := c.aead.Seal(nonce, nonce, plaintext, nil)
	return ciphertext, nil
}

//nolint:revive
func (c *Cipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) < c.aead.NonceSize() {
		return nil, fmt.Errorf("decrypt: %w", errors.ErrCiphertextTooShort)
	}

	nonce := ciphertext[:c.aead.NonceSize()]
	encrypted := ciphertext[c.aead.NonceSize():]

	plaintext, err := c.aead.Open(nil, nonce, encrypted, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %w", err)
	}
	return plaintext, nil
}
