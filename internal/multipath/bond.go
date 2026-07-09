// Package multipath implements Bond, an aggregator that splits the olcrtc
// tunnel across N carrier connections ("paths") and stripes traffic across
// them for combined throughput and per-path fault tolerance.
//
// Bond itself satisfies transport.Transport, so it slots in wherever a
// single carrier is expected today - in particular
// muxconn.New(bond, cipher) works unmodified, because muxconn only ever
// calls the transport.Transport methods (Send/Close/CanSend/...) and relies
// on the caller wiring Push as the OnData sink (see Bond.SetOnData).
//
// Each path carries its own Bond-protocol framing (frame.go): PATH_HELLO
// announces a path joining the bond, DATA carries one striped chunk with a
// bond-wide sequence number, and ACK carries a cumulative sequence so the
// sender can trim its resend buffer. Bond does not implement its own
// retransmit-on-loss - the underlying carriers (KCP/SCTP based transports)
// already guarantee in-order, reliable delivery for as long as a given path
// is alive. Bond only reroutes frames that were in flight on a path that
// died outright, and dedups on the receive side by sequence number.
package multipath

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// Role identifies which side of the tunnel a Bond represents. It only
// affects bookkeeping/logging today; Phase 2's server-side bond registry is
// expected to use it to decide whether a bond accepts late-joining paths.
type Role int

const (
	// RoleClient is a bond built from paths the local process dialed out.
	RoleClient Role = iota
	// RoleServer is a bond built by a server-side registry that accepts
	// paths as they arrive (see AddPath).
	RoleServer
)

// String renders the role for logging.
func (r Role) String() string {
	switch r {
	case RoleClient:
		return "client"
	case RoleServer:
		return "server"
	default:
		return "unknown"
	}
}

// ErrNoAlivePath is returned by Send when no path is currently usable.
var ErrNoAlivePath = errors.New("multipath: no alive path")

// ErrBondClosed is returned by Send/Connect after Close.
var ErrBondClosed = errors.New("multipath: bond closed")

// ackInterval is how often Bond re-announces its current cumulative ack, as
// a backstop against the specific ACK frame having been lost because the
// path carrying it died before delivery.
const ackInterval = 100 * time.Millisecond

// pendingFrame is a DATA frame the sender has shipped but has not yet seen
// cumulatively acked. Kept so it can be rerouted if the path it went out on
// dies before the peer acks it.
type pendingFrame struct {
	seq       uint64
	frame     []byte
	pathIndex uint16
}

// Bond aggregates N transport.Transport paths into a single logical
// transport.Transport.
type Bond struct {
	id   [16]byte
	role Role

	scheduler Scheduler
	manager   PathManager

	mu      sync.RWMutex
	paths   map[uint16]*path
	closed  bool
	started bool

	onDataMu sync.RWMutex
	onData   func([]byte)

	// control-plane proxy state (see control.go). controlOnData is the single
	// callback shared by every control-capable path so a control frame arriving
	// on any path is delivered up exactly once. controlSelected/controlHasSel
	// implement sticky path selection for ControlSend: the control/handshake
	// stream is a single ordered smux stream, so it must ride one path and only
	// fail over when that path dies - never stripe, which would reorder it.
	// Lock ordering: controlMu is always acquired before b.mu, never after.
	controlMu       sync.Mutex
	controlOnData   func([]byte)
	controlSelected uint16
	controlHasSel   bool

	cbMu            sync.Mutex
	reconnectCb     func()
	endedCb         func(string)
	shouldReconnect func() bool
	wasAlive        atomic.Bool

	sendSeq atomic.Uint64

	sendMu     sync.Mutex
	pending    map[uint64]*pendingFrame
	peerCumAck uint64

	recvMu      sync.Mutex
	expectedSeq uint64
	reorderBuf  map[uint64][]byte
	lastAckSent uint64

	closeOnce sync.Once
	closeCh   chan struct{}
}

// Option configures optional Bond behaviour at construction time.
type Option func(*Bond)

// WithScheduler overrides the default round-robin Scheduler.
func WithScheduler(s Scheduler) Option {
	return func(b *Bond) { b.scheduler = s }
}

// WithPathManager overrides the default ManualPathManager.
func WithPathManager(m PathManager) Option {
	return func(b *Bond) { b.manager = m }
}

// NewBondID generates a random bond identifier for PATH_HELLO framing.
func NewBondID() [16]byte {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		// crypto/rand failure is effectively unheard of on supported
		// platforms; fall back to a time-derived value rather than panic
		// a long-lived process over it.
		binary.BigEndian.PutUint64(id[:8], uint64(time.Now().UnixNano()))   //nolint:gosec // fallback uniqueness only
		binary.BigEndian.PutUint64(id[8:], uint64(time.Now().UnixNano())+1) //nolint:gosec // fallback uniqueness only
	}
	return id
}

// NewBond creates an empty Bond. In client mode, call AddPath for each
// already-constructed transport.Transport path before/at Connect. In server
// mode, AddPath is called by a bond registry as paths arrive - Connect need
// not be called at all in that case (each path is expected to already be
// connected by whatever accepted it).
func NewBond(id [16]byte, role Role, opts ...Option) *Bond {
	b := &Bond{
		id:          id,
		role:        role,
		scheduler:   NewRoundRobinScheduler(),
		manager:     NewManualPathManager(0),
		paths:       make(map[uint16]*path),
		pending:     make(map[uint64]*pendingFrame),
		expectedSeq: 1,
		reorderBuf:  make(map[uint64][]byte),
		closeCh:     make(chan struct{}),
	}
	for _, opt := range opts {
		opt(b)
	}
	go b.ackLoop()
	return b
}

// NewClientBond is a convenience constructor for the common client case:
// paths are already constructed (their Config.OnData should already route
// to bond.PathOnData(index) - see PathOnData) and just need registering.
func NewClientBond(id [16]byte, paths []transport.Transport, opts ...Option) *Bond {
	b := NewBond(id, RoleClient, opts...)
	for i, tr := range paths {
		b.AddPath(tr, uint16(i)) //nolint:gosec // path counts are small, bounded by caller
	}
	return b
}

// BondID returns this bond's identifier.
func (b *Bond) BondID() [16]byte { return b.id }

// Role returns whether this bond is a client- or server-side aggregate.
func (b *Bond) Role() Role { return b.role }

// NumPaths returns the number of paths currently registered (alive or dead).
func (b *Bond) NumPaths() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.paths)
}

// PathOnData returns the callback that a path's transport.Config.OnData
// should be wired to before that path's transport is constructed. Typical
// client-side setup:
//
//	bond := multipath.NewBond(multipath.NewBondID(), multipath.RoleClient)
//	for i := range n {
//		tr, _ := transport.New(ctx, name, transport.Config{..., OnData: bond.PathOnData(uint16(i))})
//		bond.AddPath(tr, uint16(i))
//	}
func (b *Bond) PathOnData(pathIndex uint16) func([]byte) {
	return func(data []byte) { b.handleFrame(pathIndex, data) }
}

// SetOnData registers the callback Bond invokes with each reassembled,
// in-order chunk of the aggregate stream. This is the Bond-level analogue of
// transport.Config.OnData: since Bond itself must exist before anything can
// wrap it (e.g. muxconn.New(bond, cipher)), the callback is set after
// construction rather than passed in up front. Data received before
// SetOnData is called is dropped.
func (b *Bond) SetOnData(cb func([]byte)) {
	b.onDataMu.Lock()
	b.onData = cb
	b.onDataMu.Unlock()
}

// AddPath registers tr as path pathIndex. Safe to call before or after
// Connect: paths added after Connect are treated as already live (their
// HELLO is sent immediately); paths added before Connect are connected by
// Connect itself.
func (b *Bond) AddPath(tr transport.Transport, pathIndex uint16) {
	p := newPath(pathIndex, tr)

	// Bond has no ARQ of its own - it leans entirely on each path being a
	// reliable, in-order carrier (frame.go header comment). A path that is not
	// Reliable+Ordered would silently corrupt the aggregate stream, so surface
	// it loudly rather than degrade quietly.
	if f := tr.Features(); !f.Reliable || !f.Ordered {
		logger.Errorf("multipath: path %d transport is not Reliable+Ordered "+
			"(reliable=%t ordered=%t) - unsuitable as a bond path; aggregate stream may corrupt",
			pathIndex, f.Reliable, f.Ordered)
	}

	tr.SetEndedCallback(func(reason string) { b.handlePathEnded(pathIndex, reason) })
	tr.SetReconnectCallback(func() { b.handlePathReconnected(pathIndex) })

	b.cbMu.Lock()
	shouldFn := b.shouldReconnect
	b.cbMu.Unlock()
	if shouldFn != nil {
		tr.SetShouldReconnect(shouldFn)
	}

	b.mu.Lock()
	b.paths[pathIndex] = p
	started := b.started
	b.mu.Unlock()

	b.manager.OnPathAdded(pathIndex)

	// If this path carries a control plane and a control callback was already
	// registered (SetControlOnData ran before this path joined), wire it now so
	// control frames arriving on this path reach the shared sink. Paths that
	// join before SetControlOnData are handled by setControlOnData itself, which
	// walks every present control-capable path.
	if p.control != nil {
		b.controlMu.Lock()
		ctrlCb := b.controlOnData
		b.controlMu.Unlock()
		if ctrlCb != nil {
			p.control.SetControlOnData(ctrlCb)
		}
	}

	if started {
		p.markAlive()
		b.sendHello(p)
		b.checkAliveTransition()
	}
}

// orderedPathsLocked returns paths sorted by index. Callers must hold at
// least a read lock on b.mu.
func (b *Bond) orderedPathsLocked() []*path {
	indices := make([]uint16, 0, len(b.paths))
	for idx := range b.paths {
		indices = append(indices, idx)
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	out := make([]*path, 0, len(indices))
	for _, idx := range indices {
		out = append(out, b.paths[idx])
	}
	return out
}

// Connect connects every path currently registered (paths added later via
// AddPath connect themselves before being handed to Bond, or are already
// connected - see AddPath doc). Returns an error only if every path failed
// to connect; partial failures are logged and the bond proceeds with
// whichever paths came up.
func (b *Bond) Connect(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return ErrBondClosed
	}
	b.started = true
	paths := b.orderedPathsLocked()
	b.mu.Unlock()

	if len(paths) == 0 {
		return nil
	}

	errs := make([]error, len(paths))
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		go func(i int, p *path) {
			defer wg.Done()
			if err := p.tr.Connect(ctx); err != nil {
				p.markDead()
				errs[i] = fmt.Errorf("path %d connect: %w", p.index, err)
				return
			}
			p.markAlive()
			b.sendHello(p)
		}(i, p)
	}
	wg.Wait()

	var joined error
	failed := 0
	for _, err := range errs {
		if err != nil {
			failed++
			joined = errors.Join(joined, err)
		}
	}
	b.checkAliveTransition()

	if failed == len(paths) {
		return fmt.Errorf("multipath: all %d paths failed to connect: %w", len(paths), joined)
	}
	if joined != nil {
		logger.Warnf("multipath: %d/%d paths failed to connect: %v", failed, len(paths), joined)
	}
	return nil
}

// sendHello sends a PATH_HELLO frame announcing p on itself.
func (b *Bond) sendHello(p *path) {
	b.mu.RLock()
	numPaths := len(b.paths)
	b.mu.RUnlock()
	frame := encodeHello(helloFrame{bondID: b.id, pathIndex: p.index, numPaths: uint16(numPaths)}) //nolint:gosec // path counts are small
	if err := p.send(frame); err != nil {
		logger.Warnf("multipath: path %d hello send failed: %v", p.index, err)
	}
}

// pickAlivePath asks the scheduler for the next path to use.
func (b *Bond) pickAlivePath() (*path, error) {
	b.mu.RLock()
	snapshot := b.orderedPathsLocked()
	b.mu.RUnlock()
	idx := b.scheduler.Pick(snapshot)
	if idx < 0 || idx >= len(snapshot) {
		return nil, ErrNoAlivePath
	}
	return snapshot[idx], nil
}

// Send stripes data as a single DATA frame across whichever path the
// scheduler picks, and remembers it in the resend buffer until it is
// cumulatively acked so it can be rerouted if that path dies.
func (b *Bond) Send(data []byte) error {
	b.mu.RLock()
	closed := b.closed
	b.mu.RUnlock()
	if closed {
		return ErrBondClosed
	}

	seq := b.sendSeq.Add(1)
	frame := encodeData(seq, data)

	p, err := b.pickAlivePath()
	if err != nil {
		return err
	}

	pf := &pendingFrame{seq: seq, frame: frame, pathIndex: p.index}
	b.sendMu.Lock()
	b.pending[seq] = pf
	b.sendMu.Unlock()

	if err := p.send(frame); err != nil {
		p.markDead()
		b.checkAliveTransition()
		b.requeueOne(pf)
	}
	return nil
}

// requeuePendingForPath reroutes every still-unacked frame that was last
// sent on deadIdx to another alive path, in seq order.
func (b *Bond) requeuePendingForPath(deadIdx uint16) {
	b.sendMu.Lock()
	toResend := make([]*pendingFrame, 0)
	for _, pf := range b.pending {
		if pf.pathIndex == deadIdx {
			toResend = append(toResend, pf)
		}
	}
	b.sendMu.Unlock()

	sort.Slice(toResend, func(i, j int) bool { return toResend[i].seq < toResend[j].seq })
	for _, pf := range toResend {
		b.requeueOne(pf)
	}
}

// requeueOne resends pf on a fresh alive path, trying every currently known
// path at most once so a run of simultaneously-dying paths can't recurse or
// spin forever.
func (b *Bond) requeueOne(pf *pendingFrame) {
	b.sendMu.Lock()
	_, stillPending := b.pending[pf.seq]
	b.sendMu.Unlock()
	if !stillPending {
		return
	}

	b.mu.RLock()
	attempts := len(b.paths)
	b.mu.RUnlock()
	if attempts == 0 {
		attempts = 1
	}

	for i := 0; i < attempts; i++ {
		p, err := b.pickAlivePath()
		if err != nil {
			logger.Warnf("multipath: no alive path to resend seq=%d", pf.seq)
			return
		}

		b.sendMu.Lock()
		if _, ok := b.pending[pf.seq]; !ok {
			b.sendMu.Unlock()
			return
		}
		pf.pathIndex = p.index
		b.sendMu.Unlock()

		if err := p.send(pf.frame); err == nil {
			return
		}
		p.markDead()
		b.checkAliveTransition()
	}
	logger.Warnf("multipath: failed to resend seq=%d after %d attempts", pf.seq, attempts)
}

// handleFrame demultiplexes one incoming Bond-protocol frame received on
// pathIndex.
func (b *Bond) handleFrame(pathIndex uint16, raw []byte) {
	ft, err := peekFrameType(raw)
	if err != nil {
		logger.Warnf("multipath: empty frame from path %d", pathIndex)
		return
	}
	switch ft {
	case frameTypeHello:
		hello, err := decodeHello(raw)
		if err != nil {
			logger.Warnf("multipath: bad hello from path %d: %v", pathIndex, err)
			return
		}
		logger.Infof("multipath: path %d hello bond=%x index=%d numPaths=%d",
			pathIndex, hello.bondID, hello.pathIndex, hello.numPaths)
	case frameTypeData:
		seq, payload, err := decodeData(raw)
		if err != nil {
			logger.Warnf("multipath: bad data frame from path %d: %v", pathIndex, err)
			return
		}
		b.handleDataFrame(seq, payload)
	case frameTypeAck:
		cumSeq, err := decodeAck(raw)
		if err != nil {
			logger.Warnf("multipath: bad ack frame from path %d: %v", pathIndex, err)
			return
		}
		b.handleAckFrame(cumSeq)
	default:
		logger.Warnf("multipath: unknown frame type %d from path %d", byte(ft), pathIndex)
	}
}

// handleDataFrame reorders and dedups incoming DATA frames, delivering
// everything now contiguous with expectedSeq up to Bond's OnData callback in
// order, then acking the new cumulative sequence.
func (b *Bond) handleDataFrame(seq uint64, payload []byte) {
	b.recvMu.Lock()
	if seq < b.expectedSeq {
		// Already delivered - duplicate from a path that got rerouted
		// around, or a stale resend. Drop.
		b.recvMu.Unlock()
		return
	}
	if seq != b.expectedSeq {
		if _, exists := b.reorderBuf[seq]; !exists {
			cp := make([]byte, len(payload))
			copy(cp, payload)
			b.reorderBuf[seq] = cp
		}
		b.recvMu.Unlock()
		return
	}

	toDeliver := [][]byte{payload}
	b.expectedSeq++
	for {
		buf, ok := b.reorderBuf[b.expectedSeq]
		if !ok {
			break
		}
		delete(b.reorderBuf, b.expectedSeq)
		toDeliver = append(toDeliver, buf)
		b.expectedSeq++
	}
	ackTo := b.expectedSeq - 1

	// Call the callback while still holding recvMu, not after releasing it.
	// Internal bookkeeping (expectedSeq/reorderBuf) is already race-free
	// under the lock, but if two paths deliver concurrently and each
	// computes its own contiguous batch, dropping the lock before invoking
	// cb would let their cb() calls race each other and arrive out of
	// sequence even though expectedSeq advanced correctly - the corruption
	// would be purely in delivery order, not in the accounting. Holding the
	// lock here serializes deliveries in the same order the sequence
	// numbers demand, at the cost of one path's delivery blocking behind
	// another's callback - an acceptable price for a strict ordering
	// guarantee.
	b.onDataMu.RLock()
	cb := b.onData
	b.onDataMu.RUnlock()
	if cb != nil {
		for _, d := range toDeliver {
			cb(d)
		}
	}
	b.recvMu.Unlock()

	b.sendAck(ackTo)
}

// handleAckFrame advances peerCumAck and trims the resend buffer.
func (b *Bond) handleAckFrame(cumSeq uint64) {
	b.sendMu.Lock()
	if cumSeq > b.peerCumAck {
		b.peerCumAck = cumSeq
	}
	for seq := range b.pending {
		if seq <= cumSeq {
			delete(b.pending, seq)
		}
	}
	b.sendMu.Unlock()
}

// sendAck sends a cumulative ACK if cumSeq advances what was last sent.
func (b *Bond) sendAck(cumSeq uint64) {
	b.recvMu.Lock()
	if cumSeq <= b.lastAckSent {
		b.recvMu.Unlock()
		return
	}
	b.lastAckSent = cumSeq
	b.recvMu.Unlock()
	b.sendAckFrame(cumSeq)
}

// sendAckFrame unconditionally sends an ACK frame for cumSeq on whichever
// path the scheduler picks.
func (b *Bond) sendAckFrame(cumSeq uint64) {
	p, err := b.pickAlivePath()
	if err != nil {
		return
	}
	if err := p.send(encodeAck(cumSeq)); err != nil {
		p.markDead()
		b.checkAliveTransition()
	}
}

// ackLoop periodically re-announces the current cumulative ack as a
// backstop against the specific ACK frame having been lost when its
// carrying path died in flight.
func (b *Bond) ackLoop() {
	ticker := time.NewTicker(ackInterval)
	defer ticker.Stop()
	for {
		select {
		case <-b.closeCh:
			return
		case <-ticker.C:
			b.recvMu.Lock()
			ackTo := b.expectedSeq - 1
			b.recvMu.Unlock()
			if ackTo > 0 {
				b.sendAckFrame(ackTo)
			}
		}
	}
}

// handlePathEnded marks pathIndex dead, reroutes its in-flight frames, and
// updates aggregate liveness.
func (b *Bond) handlePathEnded(pathIndex uint16, reason string) {
	b.mu.RLock()
	p, ok := b.paths[pathIndex]
	b.mu.RUnlock()
	if !ok {
		return
	}
	p.markDead()
	logger.Infof("multipath: path %d ended: %s", pathIndex, reason)
	b.requeuePendingForPath(pathIndex)
	b.manager.OnPathRemoved(pathIndex)
	b.checkAliveTransition()
}

// handlePathReconnected marks pathIndex alive again and re-announces it.
func (b *Bond) handlePathReconnected(pathIndex uint16) {
	b.mu.RLock()
	p, ok := b.paths[pathIndex]
	b.mu.RUnlock()
	if !ok {
		return
	}
	p.markAlive()
	b.sendHello(p)
	b.checkAliveTransition()
}

// checkAliveTransition fires reconnectCb/endedCb when CanSend flips.
func (b *Bond) checkAliveTransition() {
	alive := b.CanSend()
	was := b.wasAlive.Swap(alive)
	if alive == was {
		return
	}
	b.cbMu.Lock()
	reconnectCb := b.reconnectCb
	endedCb := b.endedCb
	b.cbMu.Unlock()
	if alive && reconnectCb != nil {
		reconnectCb()
	} else if !alive && endedCb != nil {
		endedCb("all paths ended")
	}
}

// CanSend reports whether at least one path is currently usable.
func (b *Bond) CanSend() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, p := range b.paths {
		if p.isAlive() {
			return true
		}
	}
	return false
}

// Features aggregates path features conservatively: Reliable/Ordered/
// MessageOriented are true only if every path reports them, and
// MaxPayloadSize is the smallest bound reported by any path (0 meaning
// unbounded is only kept when every path is unbounded), minus Bond's own
// DATA frame header overhead.
func (b *Bond) Features() transport.Features {
	b.mu.RLock()
	defer b.mu.RUnlock()

	agg := transport.Features{Reliable: true, Ordered: true, MessageOriented: true}
	minPayload := 0
	for _, p := range b.paths {
		f := p.tr.Features()
		agg.Reliable = agg.Reliable && f.Reliable
		agg.Ordered = agg.Ordered && f.Ordered
		agg.MessageOriented = agg.MessageOriented && f.MessageOriented
		if f.MaxPayloadSize > 0 && (minPayload == 0 || f.MaxPayloadSize < minPayload) {
			minPayload = f.MaxPayloadSize
		}
	}
	agg.MaxPayloadSize = minPayload
	if agg.MaxPayloadSize > dataFrameHeaderSize {
		agg.MaxPayloadSize -= dataFrameHeaderSize
	}
	return agg
}

// SetReconnectCallback registers cb to be invoked when the bond regains its
// first alive path after having none.
func (b *Bond) SetReconnectCallback(cb func()) {
	b.cbMu.Lock()
	b.reconnectCb = cb
	b.cbMu.Unlock()
}

// SetEndedCallback registers cb to be invoked when every path has died.
func (b *Bond) SetEndedCallback(cb func(string)) {
	b.cbMu.Lock()
	b.endedCb = cb
	b.cbMu.Unlock()
}

// SetShouldReconnect stores fn and forwards it to every path (present and
// future, via AddPath), matching the semantics of a single transport's
// SetShouldReconnect.
func (b *Bond) SetShouldReconnect(fn func() bool) {
	b.cbMu.Lock()
	b.shouldReconnect = fn
	b.cbMu.Unlock()

	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		p.tr.SetShouldReconnect(fn)
	}
}

// WatchConnection spawns a watch goroutine per path and returns immediately;
// it does not itself block, matching how callers already invoke a single
// transport's WatchConnection (`go ln.WatchConnection(ctx)`).
func (b *Bond) WatchConnection(ctx context.Context) {
	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		go p.tr.WatchConnection(ctx)
	}
}

// Reconnect asks every path to tear down and re-establish its carrier.
func (b *Bond) Reconnect(reason string) {
	b.mu.RLock()
	paths := b.orderedPathsLocked()
	b.mu.RUnlock()
	for _, p := range paths {
		p.tr.Reconnect(reason)
	}
}

// Close closes every path and stops the ack loop. Idempotent.
func (b *Bond) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	paths := b.orderedPathsLocked()
	b.mu.Unlock()

	b.closeOnce.Do(func() { close(b.closeCh) })

	var firstErr error
	for _, p := range paths {
		if err := p.tr.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

var _ transport.Transport = (*Bond)(nil)
