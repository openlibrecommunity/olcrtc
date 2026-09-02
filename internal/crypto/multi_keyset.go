package crypto

import (
	"encoding/hex"
	"fmt"
	"sync/atomic"
)

// MultiKeySet wraps multiple KeySets for decryption fallback.
// On receive (Open), tries each key in order until one succeeds, then remembers it.
// On send (Seal), uses the last successful key, or the primary (first) key if no successful open yet.
type MultiKeySet struct {
	primary   *KeySet       // first key for fallback
	keySets   []*KeySet     // all keys for receiving, in preference order
	activeIdx atomic.Uint32 // index of last successful key (for send), 0 if using primary
}

// NewMultiKeySet creates a MultiKeySet from a list of key hex strings and role.
func NewMultiKeySet(keyHexes []string, role Role) (*MultiKeySet, error) {
	if len(keyHexes) == 0 {
		return nil, ErrInvalidKeySize
	}

	keySets := make([]*KeySet, len(keyHexes))
	for i, keyHex := range keyHexes {
		key, err := hex.DecodeString(keyHex)
		if err != nil {
			return nil, fmt.Errorf("decode key %d: %w", i, err)
		}
		if len(key) != 32 {
			return nil, ErrInvalidKeySize
		}
		ks, err := NewKeySet(key, role)
		if err != nil {
			return nil, fmt.Errorf("create keyset %d: %w", i, err)
		}
		keySets[i] = ks
	}

	return &MultiKeySet{
		primary: keySets[0],
		keySets: keySets,
	}, nil
}

// getActiveKeySet returns the KeySet that should be used for encryption.
// Returns the last successful key from Open, or the primary key if no successful open yet.
func (m *MultiKeySet) getActiveKeySet() *KeySet {
	idx := m.activeIdx.Load()
	if idx < uint32(len(m.keySets)) {
		return m.keySets[idx]
	}
	return m.primary
}

// Seal encrypts using the active key (last successful from Open, or primary).
func (m *MultiKeySet) Seal(plaintext, aad []byte) ([]byte, error) {
	return m.getActiveKeySet().Seal(plaintext, aad)
}

// SealInto encrypts into dst using the active key (last successful from Open, or primary).
func (m *MultiKeySet) SealInto(dst, plaintext, aad []byte) ([]byte, error) {
	return m.getActiveKeySet().SealInto(dst, plaintext, aad)
}

// Open decrypts trying each key in order, remembers the successful one.
func (m *MultiKeySet) Open(record, aad []byte) ([]byte, error) {
	var lastErr error
	for i, ks := range m.keySets {
		pt, err := ks.Open(record, aad)
		if err == nil {
			m.activeIdx.Store(uint32(i))
			return pt, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// OpenInto decrypts into dst trying each key in order, remembers the successful one.
func (m *MultiKeySet) OpenInto(dst, record, aad []byte) ([]byte, error) {
	var lastErr error
	for i, ks := range m.keySets {
		pt, err := ks.OpenInto(dst, record, aad)
		if err == nil {
			m.activeIdx.Store(uint32(i))
			return pt, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
