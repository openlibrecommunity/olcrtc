package crypto

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeyIDLen is the length of the hex key identifier exposed in stats.
const KeyIDLen = 16

//nolint:gochecknoglobals,nolintlint // exported sentinel errors are stable API values
var (
	// ErrNoRingMatch is returned when no ring entry authenticates a record.
	ErrNoRingMatch = errors.New("no ring key opens this record")
	// ErrEmptyRing is returned when a KeyRing is built with zero keys.
	ErrEmptyRing = errors.New("ring requires at least one key")
)

// RingEntry pairs one key set with the stable identifier used for metering.
// KeyID is hex(sha256(raw 32-byte key))[:KeyIDLen], never the key itself.
type RingEntry struct {
	Keys  *KeySet
	KeyID string
}

// KeyRing is an ordered, immutable set of candidate keys a server may serve
// peers under. A peer's first record is tried against every entry; the one
// that authenticates it identifies the peer's key. The AEAD tag makes the
// trial unambiguous.
//
// ai-generated: multi-key ring over the v2 directional key sets.
type KeyRing struct {
	entries []RingEntry
}

// KeyIDFromHex derives the metering key id from a 64-hex key. The digest is
// over the decoded bytes, so the id does not depend on the hex case.
func KeyIDFromHex(keyHex string) (string, error) {
	raw, err := hex.DecodeString(keyHex)
	if err != nil {
		return "", fmt.Errorf("key id: decode hex: %w", err)
	}
	return keyID(raw), nil
}

func keyID(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:KeyIDLen]
}

// NewKeyRing builds a ring for role from 64-hex keys, keeping order and
// dropping exact duplicates. Order is trial cost: put the likely key first.
func NewKeyRing(keysHex []string, role Role) (*KeyRing, error) {
	if len(keysHex) == 0 {
		return nil, ErrEmptyRing
	}
	seen := make(map[string]struct{}, len(keysHex))
	entries := make([]RingEntry, 0, len(keysHex))
	for i, keyHex := range keysHex {
		raw, err := hex.DecodeString(keyHex)
		if err != nil {
			return nil, fmt.Errorf("ring key %d: decode hex: %w", i, err)
		}
		if _, dup := seen[string(raw)]; dup {
			continue
		}
		keys, err := NewKeySet(raw, role)
		if err != nil {
			return nil, fmt.Errorf("ring key %d: %w", i, err)
		}
		seen[string(raw)] = struct{}{}
		entries = append(entries, RingEntry{Keys: keys, KeyID: keyID(raw)})
	}
	return &KeyRing{entries: entries}, nil
}

// SingleEntry wraps an already-built key set as a one-entry ring. keyID may
// be empty when the caller has no metering identity for it.
func SingleEntry(keys *KeySet, keyID string) *KeyRing {
	return &KeyRing{entries: []RingEntry{{Keys: keys, KeyID: keyID}}}
}

// Len reports the number of entries.
func (r *KeyRing) Len() int { return len(r.entries) }

// Entries returns the ordered entries. Callers must not mutate the slice.
func (r *KeyRing) Entries() []RingEntry { return r.entries }

// TryOpen tries each entry in order and returns the plaintext (appended to
// dst) with the entry that opened the record. A record no entry
// authenticates yields ErrNoRingMatch; a record that is not a v2 record at
// all, or a replay under the key that opened it, returns that error at once.
func (r *KeyRing) TryOpen(dst, record, aad []byte) ([]byte, *RingEntry, error) {
	for i := range r.entries {
		pt, err := r.entries[i].Keys.OpenInto(dst, record, aad)
		if err == nil {
			return pt, &r.entries[i], nil
		}
		if !errors.Is(err, ErrAuthentication) {
			return nil, nil, err
		}
	}
	return nil, nil, ErrNoRingMatch
}
