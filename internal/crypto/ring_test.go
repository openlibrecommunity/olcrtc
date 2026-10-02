package crypto

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func hexKey(b byte) string { return strings.Repeat(string("0123456789abcdef"[b%16]), 64) }

func decodeHex(s string) ([]byte, error) { return hex.DecodeString(s) }

func TestNewKeyRingRejectsEmptyAndBadKeys(t *testing.T) {
	if _, err := NewKeyRing(nil, Server); !errors.Is(err, ErrEmptyRing) {
		t.Fatalf("empty ring error = %v", err)
	}
	if _, err := NewKeyRing([]string{"zz"}, Server); err == nil {
		t.Fatal("bad hex accepted")
	}
	if _, err := NewKeyRing([]string{"aabb"}, Server); !errors.Is(err, ErrInvalidKeySize) {
		t.Fatalf("short key error = %v", err)
	}
}

func TestKeyRingTryOpenPicksTheRightKeyOfThree(t *testing.T) {
	keys := []string{hexKey(1), hexKey(2), hexKey(3)}
	ring, err := NewKeyRing(keys, Server)
	if err != nil {
		t.Fatal(err)
	}
	for i, k := range keys {
		psk, _ := decodeHex(k)
		client, err := NewKeySet(psk, Client)
		if err != nil {
			t.Fatal(err)
		}
		record, err := client.Seal([]byte("hello"), []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		pt, entry, err := ring.TryOpen(nil, record, []byte("aad"))
		if err != nil || string(pt) != "hello" {
			t.Fatalf("key %d: TryOpen = %q, %v", i, pt, err)
		}
		if entry != &ring.Entries()[i] {
			t.Fatalf("key %d: matched entry %v, want index %d", i, entry.KeyID, i)
		}
	}
}

func TestKeyRingTryOpenRejectsUnknownKeyAndJunk(t *testing.T) {
	ring, _ := NewKeyRing([]string{hexKey(1)}, Server)
	psk, _ := decodeHex(hexKey(9))
	stranger, _ := NewKeySet(psk, Client)
	record, _ := stranger.Seal([]byte("x"), []byte("aad"))
	if _, _, err := ring.TryOpen(nil, record, []byte("aad")); !errors.Is(err, ErrNoRingMatch) {
		t.Fatalf("unknown key error = %v, want ErrNoRingMatch", err)
	}
	if _, _, err := ring.TryOpen(nil, make([]byte, WireOverhead+2), []byte("aad")); !errors.Is(err, ErrBadRecordMagic) {
		t.Fatalf("junk error = %v, want ErrBadRecordMagic", err)
	}
}

func TestKeyRingDedupesIdenticalKeys(t *testing.T) {
	ring, err := NewKeyRing([]string{hexKey(1), strings.ToUpper(hexKey(1))}, Server)
	if err != nil || ring.Len() != 1 {
		t.Fatalf("ring len = %d, err = %v; want 1 entry", ring.Len(), err)
	}
}

func TestKeyIDFromHexStableAndCaseInsensitive(t *testing.T) {
	a, err := KeyIDFromHex(hexKey(2))
	if err != nil || len(a) != KeyIDLen {
		t.Fatalf("KeyIDFromHex = %q, %v", a, err)
	}
	b, _ := KeyIDFromHex(strings.ToUpper(hexKey(2)))
	if a != b {
		t.Fatalf("key id differs by case: %s vs %s", a, b)
	}
	if _, err := KeyIDFromHex("zz"); err == nil {
		t.Fatal("bad hex accepted")
	}
}
