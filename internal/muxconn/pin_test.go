package muxconn

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	cryptopkg "github.com/openlibrecommunity/olcrtc/internal/crypto"
)

const (
	pskA = "01234567890123456789012345678901"
	pskB = "abcdefghijklmnopqrstuvwxyz012345"
	pskZ = "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
)

func ringOf(t *testing.T, psks ...string) *cryptopkg.KeyRing {
	t.Helper()
	hexes := make([]string, 0, len(psks))
	for _, psk := range psks {
		hexes = append(hexes, hex.EncodeToString([]byte(psk)))
	}
	ring, err := cryptopkg.NewKeyRing(hexes, cryptopkg.Server)
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func keysFor(t *testing.T, psk string, role cryptopkg.Role) *cryptopkg.KeySet {
	t.Helper()
	keys, err := cryptopkg.NewKeySet([]byte(psk), role)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func keyIDOf(t *testing.T, psk string) string {
	t.Helper()
	id, err := cryptopkg.KeyIDFromHex(hex.EncodeToString([]byte(psk)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func sealedBy(t *testing.T, keys *cryptopkg.KeySet, text string) []byte {
	t.Helper()
	record, err := keys.Seal([]byte(text), []byte(dataRecordAAD))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestGroupedConnPinsOnFirstMatchingRecord(t *testing.T) {
	group := NewPinGroup(ringOf(t, pskA, pskB))
	conn := NewGrouped(&stubLink{canSend: true}, group)
	defer func() { _ = conn.Close() }()
	if group.Pinned() != nil {
		t.Fatal("pinned before any record")
	}
	conn.Push(sealedBy(t, keysFor(t, pskB, cryptopkg.Client), "hello"))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	if got := group.KeyID(); got != keyIDOf(t, pskB) {
		t.Fatalf("pinned key id = %s, want the second entry", got)
	}
}

func TestGroupedConnDropsRecordsUnderNoRingKey(t *testing.T) {
	group := NewPinGroup(ringOf(t, pskA, pskB))
	conn := NewGrouped(&stubLink{canSend: true}, group)
	defer func() { _ = conn.Close() }()
	conn.Push(sealedBy(t, keysFor(t, pskZ, cryptopkg.Client), "stranger"))
	if group.Pinned() != nil {
		t.Fatal("a stranger's record pinned the group")
	}
	if got := conn.DecryptStats(); got.AuthFailed != 1 {
		t.Fatalf("DecryptStats = %+v, want one authentication failure", got)
	}
}

func TestGroupedConnWriteWaitsForPin(t *testing.T) {
	link := &stubLink{canSend: true}
	group := NewPinGroup(ringOf(t, pskA, pskB))
	conn := NewGrouped(link, group)
	defer func() { _ = conn.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("from server"))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Write returned %v before the group was pinned", err)
	case <-time.After(100 * time.Millisecond):
	}
	clientB := keysFor(t, pskB, cryptopkg.Client)
	conn.Push(sealedBy(t, clientB, "hi"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write = %v after the pin", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not resume after the pin")
	}
	link.mu.Lock()
	sent := link.sent
	link.mu.Unlock()
	if len(sent) != 1 {
		t.Fatalf("link received %d records, want 1", len(sent))
	}
	if pt, err := clientB.Open(sent[0], []byte(dataRecordAAD)); err != nil || string(pt) != "from server" {
		t.Fatalf("the client's key set did not open the server's record: %q, %v", pt, err)
	}
}

func TestGroupedConnWriteUnblocksOnClose(t *testing.T) {
	conn := NewGrouped(&stubLink{canSend: true}, NewPinGroup(ringOf(t, pskA)))
	done := make(chan error, 1)
	go func() {
		_, err := conn.Write([]byte("never"))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = conn.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Write after Close = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write stayed blocked after Close")
	}
}

func TestPinIsSharedAcrossGroupConns(t *testing.T) {
	group := NewPinGroup(ringOf(t, pskA, pskB))
	data := NewGrouped(&stubLink{canSend: true}, group)
	control := NewGrouped(&stubLink{canSend: true}, group)
	defer func() { _ = data.Close(); _ = control.Close() }()
	data.Push(sealedBy(t, keysFor(t, pskA, cryptopkg.Client), "pin me"))
	done := make(chan error, 1)
	go func() {
		_, err := control.Write([]byte("control"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("control Write = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the control conn did not see the data conn's pin")
	}
}

func TestPrePinnedPathMatchesTheOldBehaviour(t *testing.T) {
	client, server := newTestKeyPair(t)
	link := &stubLink{canSend: true}
	conn := New(link, server)
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte("at once")); err != nil {
		t.Fatalf("Write on a pre-pinned conn = %v", err)
	}
	link.mu.Lock()
	sent := link.sent
	link.mu.Unlock()
	if pt, err := client.Open(sent[0], []byte(dataRecordAAD)); err != nil || string(pt) != "at once" {
		t.Fatalf("record = %q, %v", pt, err)
	}
}
