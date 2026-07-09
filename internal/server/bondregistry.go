package server

import (
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/multipath"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/xtaci/smux"
)

// bondEntry is the registry record for one aggregated multipath session. A
// single Bond fans traffic across every carrier (path) that announced the same
// bond id, and one muxconn+smux+handshake stack runs on top of the Bond - not
// on any individual carrier - so N carriers collapse into one logical session.
type bondEntry struct {
	id      [16]byte
	bond    *multipath.Bond
	started bool // the muxconn+smux session over the bond has been brought up
	paths   map[uint16]struct{}
}

// getOrCreateBond returns the registry entry for id, allocating and starting a
// fresh server-role Bond on first sight. created reports whether this call was
// the one that allocated the entry (i.e. this is the bond's first path, and
// the caller must bring up the session).
//
// Connecting the empty bond only flips it into its "started" state so that
// paths added afterwards are marked alive immediately; it dials nothing, since
// each carrier arrives already connected by whatever accepted it.
func (s *Server) getOrCreateBond(id [16]byte) (*bondEntry, bool) {
	s.bondMu.Lock()
	defer s.bondMu.Unlock()
	if s.bonds == nil {
		s.bonds = make(map[[16]byte]*bondEntry)
	}
	if be := s.bonds[id]; be != nil {
		return be, false
	}
	b := multipath.NewBond(id, multipath.RoleServer)
	_ = b.Connect(s.baseCtx)
	be := &bondEntry{id: id, bond: b, paths: make(map[uint16]struct{})}
	s.bonds[id] = be
	return be, true
}

// addBondPath registers carrier tr as pathIndex on the bond, ignoring a repeat
// announcement of a path index already present (a carrier re-sending its
// PATH_HELLO must not double-register).
func (s *Server) addBondPath(be *bondEntry, tr transport.Transport, pathIndex uint16) {
	s.bondMu.Lock()
	_, dup := be.paths[pathIndex]
	if !dup {
		be.paths[pathIndex] = struct{}{}
	}
	s.bondMu.Unlock()
	if dup {
		return
	}
	be.bond.AddPath(tr, pathIndex)
	logger.Infof("multipath: bond %x path %d joined (paths=%d)", be.id, pathIndex, be.bond.NumPaths())
}

// removeBond drops the bond from the registry and closes it. Safe to call
// repeatedly; the second call is a no-op.
func (s *Server) removeBond(id [16]byte) {
	s.bondMu.Lock()
	be := s.bonds[id]
	delete(s.bonds, id)
	s.bondMu.Unlock()
	if be != nil {
		_ = be.bond.Close()
	}
}

// startBondSession brings up the single muxconn+smux session that runs over the
// whole bond and wires the bond's reassembled output into it. Because Bond
// itself satisfies transport.Transport, this reuses the ordinary data-session
// plumbing (serveSingle drives the handshake and the accept loop over
// s.session exactly as it does for a lone carrier). When every path of the
// bond dies, Bond fires its ended callback and we tear the run down.
func (s *Server) startBondSession(be *bondEntry) {
	conn := muxconn.New(be.bond, s.cipher)
	sess, err := smux.Server(conn, dataSmuxConfig(be.bond))
	if err != nil {
		logger.Warnf("multipath: smux server init failed for bond %x: %v", be.id, err)
		_ = conn.Close()
		return
	}
	be.bond.SetOnData(conn.Push)
	be.bond.SetEndedCallback(func(reason string) {
		logger.Infof("multipath: bond %x ended (%s) - tearing down session", be.id, reason)
		s.removeBond(be.id)
		if s.bondCancel != nil {
			s.bondCancel()
		}
	})

	s.bondMu.Lock()
	be.started = true
	s.bondMu.Unlock()

	s.sessMu.Lock()
	s.conn = conn
	s.session = sess
	s.sessMu.Unlock()
	logger.Infof("multipath: bond %x session started", be.id)
}

// routeCarrierFrame is the multipath data path: it classifies the carrier's
// first frame and then forwards every frame through the sink that
// classification selected.
//
// The first PATH_HELLO tells us which bond this carrier belongs to; from then
// on frames are handed to that bond's path sink (which itself expects to
// receive the PATH_HELLO first, so the classifying frame is forwarded too and
// never lost). A first frame that is not a PATH_HELLO means a legacy,
// non-bonded client: we fall back to a plain single-carrier session over the
// raw carrier so old clients keep working with the flag enabled.
func (s *Server) routeCarrierFrame(data []byte) {
	if r := s.bondRouter.Load(); r != nil {
		(*r)(data)
		return
	}

	id, idx, _, ok := multipath.ParsePathHello(data)
	if !ok {
		s.installSession()
		legacy := func(b []byte) {
			s.sessMu.RLock()
			c := s.conn
			s.sessMu.RUnlock()
			if c != nil {
				c.Push(b)
			}
		}
		s.bondRouter.Store(&legacy)
		legacy(data)
		return
	}

	be, created := s.getOrCreateBond(id)
	s.addBondPath(be, s.ln, idx)
	if created {
		s.startBondSession(be)
	}
	sink := be.bond.PathOnData(idx)
	s.bondRouter.Store(&sink)
	sink(data)
}
