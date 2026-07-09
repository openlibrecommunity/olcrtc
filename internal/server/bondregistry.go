package server

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/control"
	"github.com/openlibrecommunity/olcrtc/internal/framing"
	"github.com/openlibrecommunity/olcrtc/internal/handshake"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/multipath"
	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
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

	// peer marks a Phase-5b bond whose paths are peer-routed over carriers
	// shared with other clients. Such a bond runs its own dedicated session
	// (below) with its own accept loop, isolated from every other client's
	// bond, rather than the singleton s.session used by 5a.
	peer    bool
	carrier transport.Transport // multipath.AsCarrier(bond), for liveness tuning
	sess    *peerSession        // per-bond session state (peer bonds only)
}

// maxBufferedPeerControl bounds how many pre-hello control frames a single peer
// may queue while its data-plane PATH_HELLO is still in flight (control and data
// ride separate KCP sessions, so either may arrive first). It is generous enough
// for a full handshake burst but capped so a peer that never sends a hello can
// not grow this without bound.
const maxBufferedPeerControl = 64

// peerBondPath is the resolved route for one (carrier,peerID) pair: which bond
// path inbound frames from that peer belong to.
type peerBondPath struct {
	be      *bondEntry
	pathIdx uint16
}

// peerCarrierDemux demultiplexes a single peer-routing carrier's shared
// OnPeerData / OnPeerControlData callbacks (one callback per carrier, for every
// peer) into per-(carrier,peer) bond paths. The first data frame from a peer is
// its PATH_HELLO, which resolves that peer to a bond+pathIdx; every later frame
// from the same peer is forwarded to that bond's path sink.
type peerCarrierDemux struct {
	s  *Server
	tr transport.PeerTransport // set after transport.New once peer-routing is confirmed

	mu     sync.Mutex
	routes map[string]*peerBondPath // peerID -> resolved route (nil until PATH_HELLO)
	ctlBuf map[string][][]byte      // peerID -> control frames buffered pre-hello
}

func newPeerCarrierDemux(s *Server) *peerCarrierDemux {
	return &peerCarrierDemux{
		s:      s,
		routes: make(map[string]*peerBondPath),
		ctlBuf: make(map[string][][]byte),
	}
}

// routePeerBondData handles one inbound data-plane frame for peerID on this
// carrier. The peer's first frame must be a PATH_HELLO announcing its bond id
// and path index; it resolves the peer to a bond path, brings the bond's session
// up on the first path, and flushes any control frames that arrived ahead of the
// hello. Every subsequent frame is fed to the bond's per-path sink.
func (s *Server) routePeerBondData(d *peerCarrierDemux, peerID string, data []byte) {
	if d.tr == nil {
		// The transport delivered peer data without reporting SupportsPeerRouting
		// (so we never recorded the PeerTransport for reverse addressing). Nothing
		// we can safely wire it to; drop rather than nil-deref.
		logger.Warnf("multipath/peer: peer data for %s on a carrier without confirmed peer routing; dropping", peerID)
		return
	}

	d.mu.Lock()
	route := d.routes[peerID]
	d.mu.Unlock()
	if route != nil {
		route.be.bond.PathOnData(route.pathIdx)(data)
		return
	}

	id, idx, _, ok := multipath.ParsePathHello(data)
	if !ok {
		logger.Warnf("multipath/peer: first frame from peer %s is not a PATH_HELLO; dropping", peerID)
		return
	}

	be, created := s.getOrCreateBond(id)
	s.addBondPeerPath(be, d.tr, peerID, idx)
	// Bring the bond's session up (once, on its first path) BEFORE publishing
	// the route, so the shared control receive sink (installed by
	// startPeerBondSession) exists before any control frame is routed via
	// ControlIn - otherwise a racing control frame would be silently dropped.
	if created {
		s.startPeerBondSession(be)
	}

	d.mu.Lock()
	d.routes[peerID] = &peerBondPath{be: be, pathIdx: idx}
	buffered := d.ctlBuf[peerID]
	delete(d.ctlBuf, peerID)
	d.mu.Unlock()

	be.bond.PathOnData(idx)(data)
	for _, cf := range buffered {
		be.bond.ControlIn(cf)
	}
}

// routePeerBondControl handles one inbound control-plane frame for peerID. If
// the peer's PATH_HELLO has not been seen yet (control raced ahead of data) the
// frame is buffered and flushed once routePeerBondData resolves the route.
func (s *Server) routePeerBondControl(d *peerCarrierDemux, peerID string, data []byte) {
	d.mu.Lock()
	route := d.routes[peerID]
	if route == nil {
		if len(d.ctlBuf[peerID]) < maxBufferedPeerControl {
			d.ctlBuf[peerID] = append(d.ctlBuf[peerID], append([]byte(nil), data...))
		}
		d.mu.Unlock()
		return
	}
	be := route.be
	d.mu.Unlock()
	be.bond.ControlIn(data)
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

// addBondPeerPath registers a peer-routed carrier as pathIndex on the bond
// (Phase 5b), ignoring a repeat announcement of an index already present. The
// underlying carrier is shared across bonds and closed by the server, so the
// bond only records the (carrier,peerID) pair for reverse addressing.
func (s *Server) addBondPeerPath(be *bondEntry, pt transport.PeerTransport, peerID string, pathIndex uint16) {
	s.bondMu.Lock()
	_, dup := be.paths[pathIndex]
	if !dup {
		be.paths[pathIndex] = struct{}{}
		be.peer = true
	}
	s.bondMu.Unlock()
	if dup {
		return
	}
	be.bond.AddPeerPath(pt, peerID, pathIndex)
	logger.Infof("multipath: bond %x peer path %d joined peer=%s (paths=%d)",
		be.id, pathIndex, peerID, be.bond.NumPaths())
}

// removeBond drops the bond from the registry and closes it and (for peer bonds)
// its dedicated session. Safe to call repeatedly; the second call is a no-op.
// Closing the bond never closes the shared peer carriers (see path.closeTransport).
func (s *Server) removeBond(id [16]byte) {
	s.bondMu.Lock()
	be := s.bonds[id]
	delete(s.bonds, id)
	s.bondMu.Unlock()
	if be == nil {
		return
	}
	if be.sess != nil {
		s.closePeerSession(be.sess, "closed")
	}
	_ = be.bond.Close()
}

// startBondSession brings up the single muxconn+smux session that runs over the
// whole bond and wires the bond's reassembled output into it. Because Bond
// itself satisfies transport.Transport, this reuses the ordinary data-session
// plumbing (serveSingle drives the handshake and the accept loop over
// s.session exactly as it does for a lone carrier). When every path of the
// bond dies, Bond fires its ended callback and we tear the run down.
func (s *Server) startBondSession(be *bondEntry) {
	// AsCarrier returns a control-capable wrapper only when every path of the
	// bond exposes an isolated control plane (vp8channel); otherwise the bare
	// bond, for which muxconn.NewControl below returns nil and the handshake
	// stays inline on the data session (serveSingle drives it). Paths are
	// homogeneous, so classifying on the first path is sound.
	carrier := multipath.AsCarrier(be.bond)

	conn := muxconn.New(carrier, s.cipher)
	sess, err := smux.Server(conn, dataSmuxConfig(carrier))
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

	// When the bond exposes an isolated control plane, build a dedicated
	// control smux session over it and launch the handshake acceptor - mirror
	// of installSession (server.go). serveSingle then sees s.controlConn != nil
	// and spin-waits for this goroutine instead of driving the handshake on the
	// data session, so handshake+liveness ride the priority control channel and
	// do not head-of-line block behind bulk data.
	controlConn := muxconn.NewControl(carrier, s.cipher)
	var ctrlSess *smux.Session
	if controlConn != nil {
		controlSess, cerr := smux.Server(controlConn, controlSmuxConfig(linkMaxPayload(carrier)))
		if cerr != nil {
			logger.Warnf("multipath: control smux server init failed for bond %x: %v", be.id, cerr)
			_ = controlConn.Close()
			controlConn = nil
		} else {
			ctrlSess = controlSess
			go s.acceptHandshake(s.baseCtx, controlSess)
		}
	}

	s.bondMu.Lock()
	be.started = true
	s.bondMu.Unlock()

	s.sessMu.Lock()
	s.conn = conn
	s.controlConn = controlConn
	s.controlSess = ctrlSess
	s.session = sess
	s.sessMu.Unlock()
	logger.Infof("multipath: bond %x session started (control=%t)", be.id, ctrlSess != nil)
}

// startPeerBondSession brings up the dedicated muxconn+smux session for one
// peer-routed bond (Phase 5b). Unlike startBondSession it stores the session in
// the bondEntry (not the singleton s.session) and launches per-bond accept /
// handshake goroutines, so several clients sharing the SFU-room carriers each
// get an isolated session. The bond's ended callback only tears down this bond,
// never the whole server (the carriers outlive any one client).
func (s *Server) startPeerBondSession(be *bondEntry) {
	carrier := multipath.AsCarrier(be.bond)

	conn := muxconn.New(carrier, s.cipher)
	sess, err := smux.Server(conn, dataSmuxConfig(carrier))
	if err != nil {
		logger.Warnf("multipath: smux server init failed for peer bond %x: %v", be.id, err)
		_ = conn.Close()
		return
	}
	be.bond.SetOnData(conn.Push)
	be.bond.SetEndedCallback(func(reason string) {
		logger.Infof("multipath: peer bond %x ended (%s) - tearing down its session", be.id, reason)
		s.removeBond(be.id)
	})

	controlConn := muxconn.NewControl(carrier, s.cipher)
	var ctrlSess *smux.Session
	if controlConn != nil {
		cs, cerr := smux.Server(controlConn, controlSmuxConfig(linkMaxPayload(carrier)))
		if cerr != nil {
			logger.Warnf("multipath: control smux server init failed for peer bond %x: %v", be.id, cerr)
			_ = controlConn.Close()
			controlConn = nil
		} else {
			ctrlSess = cs
		}
	}

	ps := &peerSession{
		peerID:      "bond:" + hex.EncodeToString(be.id[:]),
		conn:        conn,
		session:     sess,
		controlConn: controlConn,
		controlSess: ctrlSess,
	}
	if ctrlSess != nil {
		// Isolated control plane: the handshake runs on the control session and
		// signals readiness via sessionReady, exactly like acceptPeerHandshake.
		ps.sessionReady = make(chan struct{})
	}

	s.bondMu.Lock()
	be.started = true
	be.carrier = carrier
	be.sess = ps
	s.bondMu.Unlock()

	logger.Infof("multipath: peer bond %x session started (control=%t)", be.id, ctrlSess != nil)

	if ctrlSess != nil {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.acceptBondHandshake(be)
		}()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.serveBond(be)
	}()
}

// acceptBondHandshake runs the handshake on a peer bond's control session and
// then starts its liveness loop. Mirrors acceptPeerHandshake but writes into the
// bondEntry's session and tears the bond (not the server) down on failure.
func (s *Server) acceptBondHandshake(be *bondEntry) {
	ps := be.sess
	const maxStaleRetries = 3
	for retry := 0; retry <= maxStaleRetries; retry++ {
		stream, err := ps.controlSess.AcceptStream()
		if err != nil {
			if s.baseCtx.Err() != nil {
				return
			}
			logger.Infof("server: AcceptStream(bond control=%x) error: %v", be.id, err)
			s.removeBond(be.id)
			return
		}
		_ = stream.SetDeadline(time.Now().Add(handshake.DefaultTimeout))
		hello, sid, err := handshake.Server(stream, s.authHook)
		_ = stream.SetDeadline(time.Time{})
		if err != nil {
			_ = stream.Close()
			if errors.Is(err, framing.ErrFrameTooLarge) && retry < maxStaleRetries {
				logger.Debugf("handshake bond=%x: stale stream retry %d: %v", be.id, retry+1, err)
				continue
			}
			logger.Warnf("handshake bond=%x failed: %v", be.id, err)
			s.removeBond(be.id)
			return
		}
		s.sessMu.Lock()
		ps.deviceID = hello.DeviceID
		ps.sessionID = sid
		s.sessMu.Unlock()
		if ps.sessionReady != nil {
			close(ps.sessionReady)
		}
		s.recordSession(sid)
		s.onOpen(sid, hello.DeviceID, hello.Claims)
		s.trackPeerOpen(sid, hello.DeviceID)
		logger.Infof("bond session %s opened (bond=%x device=%s)", sid, be.id, hello.DeviceID)
		s.startBondControlLoop(be, stream)
		return
	}
}

// startBondControlLoop launches the liveness ping/pong loop for a peer bond's
// control stream, mirroring startPeerControlLoop. On loss it removes the bond.
func (s *Server) startBondControlLoop(be *bondEntry, stream *smux.Stream) {
	ps := be.sess
	controlCtx, stop := context.WithCancel(s.baseCtx)
	s.sessMu.Lock()
	ps.controlStrm = stream
	ps.controlStop = stop
	s.sessMu.Unlock()

	liveness := s.liveness
	if be.carrier != nil && runtime.IsControlPlane(be.carrier) && liveness.Timeout <= control.DefaultTimeout {
		liveness.Timeout = runtime.LivenessTimeout(be.carrier)
	}
	onPong := liveness.OnPong
	onMissedPong := liveness.OnMissedPong
	onUnhealthy := liveness.OnUnhealthy
	liveness.OnPong = func(h control.Health) {
		s.recordPong(h)
		logger.Debugf("control alive bond=%x rtt=%v seq=%d", be.id, h.RTT, h.Seq)
		if onPong != nil {
			onPong(h)
		}
	}
	liveness.OnMissedPong = func(missed int) {
		s.recordMissed(missed)
		logger.Warnf("control missed pong bond=%x missed=%d", be.id, missed)
		if onMissedPong != nil {
			onMissedPong(missed)
		}
	}
	liveness.OnUnhealthy = func(missed int) {
		s.recordUnhealthy(missed)
		logger.Warnf("control unhealthy bond=%x missed=%d", be.id, missed)
		if onUnhealthy != nil {
			onUnhealthy(missed)
		}
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { _ = stream.Close() }()
		err := control.Run(controlCtx, stream, liveness)
		if controlCtx.Err() != nil || s.baseCtx.Err() != nil {
			return
		}
		if err != nil {
			logger.Warnf("bond control stream ended bond=%x: %v", be.id, err)
		}
		s.removeBond(be.id)
	}()
}

// serveBond drives the accept loop for one peer bond's data session, mirroring
// servePeer. It first ensures the handshake has completed (via the control
// session when present, or inline on the data session otherwise).
func (s *Server) serveBond(be *bondEntry) {
	ps := be.sess
	if ps.sessionID == "" && !s.establishBondSession(be) {
		return
	}
	for {
		if s.stopping() {
			return
		}
		stream, err := ps.session.AcceptStream()
		if err != nil {
			if s.stopping() {
				return
			}
			logger.Infof("server: AcceptStream(bond=%x) error - closing bond session: %v", be.id, err)
			s.removeBond(be.id)
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleStream(context.Background(), stream, ps.sessionID)
		}()
	}
}

// establishBondSession ensures the peer bond's handshake has completed. When a
// control session exists, acceptBondHandshake signals ps.sessionReady; otherwise
// the handshake is driven inline on the data session.
func (s *Server) establishBondSession(be *bondEntry) bool {
	ps := be.sess
	if ps.sessionReady != nil {
		return s.waitBondHandshake(be)
	}
	return s.acceptBondHandshakeInline(be)
}

// waitBondHandshake blocks until acceptBondHandshake closes ps.sessionReady (and
// fills in sessionID), or the server shuts down.
func (s *Server) waitBondHandshake(be *bondEntry) bool {
	ps := be.sess
	ready := ps.sessionReady
	if ready == nil {
		return false
	}
	select {
	case <-ready:
		s.sessMu.RLock()
		sid := ps.sessionID
		s.sessMu.RUnlock()
		return sid != ""
	case <-s.done:
		s.removeBond(be.id)
		return false
	}
}

// acceptBondHandshakeInline runs the handshake on the peer bond's data session
// (used only when the carrier has no isolated per-peer control plane), writes the
// session id / device id into the bondEntry, and starts the bond liveness loop.
// On failure it tears the bond down.
func (s *Server) acceptBondHandshakeInline(be *bondEntry) bool {
	ps := be.sess
	const maxStaleRetries = 3
	for retry := 0; retry <= maxStaleRetries; retry++ {
		stream, err := ps.session.AcceptStream()
		if err != nil {
			if s.baseCtx.Err() != nil {
				return false
			}
			logger.Infof("server: AcceptStream(bond=%x inline) error: %v", be.id, err)
			s.removeBond(be.id)
			return false
		}
		_ = stream.SetDeadline(time.Now().Add(handshake.DefaultTimeout))
		hello, sid, err := handshake.Server(stream, s.authHook)
		_ = stream.SetDeadline(time.Time{})
		if err != nil {
			_ = stream.Close()
			if errors.Is(err, framing.ErrFrameTooLarge) && retry < maxStaleRetries {
				continue
			}
			logger.Warnf("handshake bond=%x (inline) failed: %v", be.id, err)
			s.removeBond(be.id)
			return false
		}
		s.sessMu.Lock()
		ps.deviceID = hello.DeviceID
		ps.sessionID = sid
		s.sessMu.Unlock()
		s.recordSession(sid)
		s.onOpen(sid, hello.DeviceID, hello.Claims)
		s.trackPeerOpen(sid, hello.DeviceID)
		logger.Infof("bond session %s opened inline (bond=%x device=%s)", sid, be.id, hello.DeviceID)
		s.startBondControlLoop(be, stream)
		return true
	}
	return false
}

// routeBondFrame is the multipath data path for a single carrier tr: it
// classifies that carrier's first frame and then forwards every frame through
// the sink that classification selected. router is that carrier's own
// first-frame classification cell (each carrier keeps its own, so N carriers
// classify independently).
//
// The first PATH_HELLO tells us which bond this carrier belongs to; from then
// on frames are handed to that bond's path sink (which itself expects to
// receive the PATH_HELLO first, so the classifying frame is forwarded too and
// never lost). A first frame that is not a PATH_HELLO is handled by onNotHello,
// which the caller supplies (legacy single-carrier fallback for the lone
// carrier; a drop sink for explicit multi-carrier deployments).
func (s *Server) routeBondFrame(
	tr transport.Transport,
	router *atomic.Pointer[func([]byte)],
	data []byte,
	onNotHello func([]byte),
) {
	if r := router.Load(); r != nil {
		(*r)(data)
		return
	}

	id, idx, _, ok := multipath.ParsePathHello(data)
	if !ok {
		onNotHello(data)
		return
	}

	be, created := s.getOrCreateBond(id)
	s.addBondPath(be, tr, idx)
	if created {
		s.startBondSession(be)
	}
	sink := be.bond.PathOnData(idx)
	router.Store(&sink)
	sink(data)
}

// routeCarrierFrame is the single-carrier multipath entry point (s.onData). It
// classifies s.ln's frames and, for a first frame that is not a PATH_HELLO,
// falls back to a plain single-carrier session over the raw carrier so a legacy
// (non-bonded) client keeps working with EnableMultipath set.
func (s *Server) routeCarrierFrame(data []byte) {
	s.routeBondFrame(s.ln, &s.bondRouter, data, func(d []byte) {
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
		legacy(d)
	})
}
