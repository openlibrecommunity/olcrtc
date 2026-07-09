package server

import (
	"context"
	"testing"
)

// TestBondRegistry_GroupAndDedup verifies that carriers announcing the same
// bond id are aggregated into one Bond (created only once) and that a repeated
// path index is ignored, while a fresh id allocates a separate bond.
func TestBondRegistry_GroupAndDedup(t *testing.T) {
	s := &Server{baseCtx: context.Background()}

	id := [16]byte{0xAA}

	be1, created1 := s.getOrCreateBond(id)
	if !created1 {
		t.Fatal("first getOrCreateBond should report created=true")
	}
	be2, created2 := s.getOrCreateBond(id)
	if created2 {
		t.Fatal("second getOrCreateBond for same id should report created=false")
	}
	if be1 != be2 {
		t.Fatal("same bond id returned different entries")
	}

	s.addBondPath(be1, &serverLinkStub{}, 0)
	s.addBondPath(be1, &serverLinkStub{}, 1)
	s.addBondPath(be1, &serverLinkStub{}, 1) // dup index - must be ignored
	if got := be1.bond.NumPaths(); got != 2 {
		t.Fatalf("bond NumPaths = %d, want 2 (dup index must not add a path)", got)
	}

	other := [16]byte{0xBB}
	beOther, createdOther := s.getOrCreateBond(other)
	if !createdOther {
		t.Fatal("distinct bond id should allocate a new entry")
	}
	if beOther == be1 {
		t.Fatal("distinct bond ids must not share an entry")
	}
}

// TestBondRegistry_Remove verifies removeBond drops the entry and closes the
// underlying bond (and is safe to call twice).
func TestBondRegistry_Remove(t *testing.T) {
	s := &Server{baseCtx: context.Background()}
	id := [16]byte{0xCC}

	be, _ := s.getOrCreateBond(id)
	stub := &serverLinkStub{}
	s.addBondPath(be, stub, 0)

	s.removeBond(id)
	if _, ok := s.bonds[id]; ok {
		t.Fatal("removeBond left the entry in the registry")
	}
	if !stub.closed {
		t.Fatal("removeBond did not close the bond's path transport")
	}
	s.removeBond(id) // idempotent
}
