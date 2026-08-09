package multipath

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// errMockDead/errMockNotConnected/errMockBufferFull are the errors a
// mockTransport's Send can fail with, mirroring the possible failure modes
// a real carrier's Send would report.
var (
	errMockDead         = errors.New("mocktransport: dead")
	errMockNotConnected = errors.New("mocktransport: not connected")
	errMockBufferFull   = errors.New("mocktransport: send buffer full")
)

// mockTransport is an in-memory transport.Transport used to exercise Bond
// without any real network/WebRTC carrier. Two mockTransports are linked
// into a loopback pair with linkMockPair; each keeps its own FIFO send
// queue drained by a dedicated goroutine, so - like a real KCP/SCTP backed
// path - frames sent on one mockTransport arrive at its peer strictly in
// order for as long as the mockTransport is alive.
type mockTransport struct {
	name string

	mu                sync.Mutex
	onData            func([]byte)
	peer              *mockTransport
	endedCb           func(string)
	reconnectCb       func()
	shouldReconnectFn func() bool

	connected boolFlag
	closed    boolFlag
	killed    boolFlag

	sendCh   chan []byte
	stopCh   chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	// delay artificially delays delivery of each frame, useful for forcing
	// cross-path reordering in tests.
	delay time.Duration
}

// boolFlag is a tiny atomic bool wrapper - internal/multipath's real code
// uses atomic.Bool directly, but tests predate importing sync/atomic here
// for just three flags; kept as a thin helper for readability.
type boolFlag struct {
	mu sync.Mutex
	v  bool
}

func (f *boolFlag) set(v bool) {
	f.mu.Lock()
	f.v = v
	f.mu.Unlock()
}

func (f *boolFlag) get() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}

func (f *boolFlag) compareAndSwap(old, newVal bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.v != old {
		return false
	}
	f.v = newVal
	return true
}

// newMockTransport creates an unconnected, unlinked mock transport.
// mockSendBuffer is generous on purpose: tests drive thousands of Send
// calls back-to-back from a single goroutine, and the point of these tests
// is to exercise Bond's ordering/dedup/requeue logic, not to model
// backpressure. A small buffer would turn "consumer momentarily behind
// producer" into spurious path deaths.
const mockSendBuffer = 8192

func newMockTransport(name string) *mockTransport {
	return &mockTransport{
		name:   name,
		sendCh: make(chan []byte, mockSendBuffer),
		stopCh: make(chan struct{}),
	}
}

// linkMockPair connects two mock transports so each one's Send delivers to
// the other's OnData callback.
func linkMockPair(a, b *mockTransport) {
	a.peer = b
	b.peer = a
}

// setOnData wires the callback invoked for every frame this mock delivers -
// tests point it at bond.PathOnData(pathIndex).
func (m *mockTransport) setOnData(cb func([]byte)) {
	m.mu.Lock()
	m.onData = cb
	m.mu.Unlock()
}

func (m *mockTransport) Connect(context.Context) error {
	m.connected.set(true)
	m.wg.Add(1)
	go m.deliverLoop()
	return nil
}

func (m *mockTransport) deliverLoop() {
	defer m.wg.Done()
	for {
		select {
		case data, ok := <-m.sendCh:
			if !ok {
				return
			}
			if m.delay > 0 {
				time.Sleep(m.delay)
			}
			m.deliver(data)
		case <-m.stopCh:
			return
		}
	}
}

func (m *mockTransport) deliver(data []byte) {
	peer := m.peer
	if peer == nil {
		return
	}
	peer.mu.Lock()
	cb := peer.onData
	peer.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

func (m *mockTransport) Send(data []byte) error {
	if m.closed.get() || m.killed.get() {
		return errMockDead
	}
	if !m.connected.get() {
		return errMockNotConnected
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case m.sendCh <- cp:
		return nil
	default:
		return errMockBufferFull
	}
}

func (m *mockTransport) Close() error {
	m.closed.set(true)
	m.stop()
	return nil
}

// kill simulates the underlying carrier dying outright: it stops delivering
// any further frames (in-flight ones are lost, matching a real dead link)
// and fires the ended callback, same as a real transport would via
// SetEndedCallback.
func (m *mockTransport) kill(reason string) {
	if !m.killed.compareAndSwap(false, true) {
		return
	}
	m.connected.set(false)
	m.stop()
	m.mu.Lock()
	cb := m.endedCb
	m.mu.Unlock()
	if cb != nil {
		cb(reason)
	}
}

// blackhole simulates a carrier dying SILENTLY: it stops delivering frames
// (in-flight ones are lost) and reports not-sendable, but never fires the
// ended callback - the "blackholed carrier whose liveness has not yet fired"
// case that Bond's time-based reaper exists to recover from. Unlike kill, it
// announces nothing.
//
// ai-generated: blackhole mode for the silent-death recovery test.
func (m *mockTransport) blackhole() {
	if !m.killed.compareAndSwap(false, true) {
		return
	}
	m.connected.set(false)
	m.stop()
}

func (m *mockTransport) stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

func (m *mockTransport) SetReconnectCallback(cb func()) {
	m.mu.Lock()
	m.reconnectCb = cb
	m.mu.Unlock()
}

func (m *mockTransport) SetShouldReconnect(fn func() bool) {
	m.mu.Lock()
	m.shouldReconnectFn = fn
	m.mu.Unlock()
}

func (m *mockTransport) SetEndedCallback(cb func(string)) {
	m.mu.Lock()
	m.endedCb = cb
	m.mu.Unlock()
}

func (m *mockTransport) WatchConnection(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-m.stopCh:
	}
}

func (m *mockTransport) CanSend() bool {
	return m.connected.get() && !m.closed.get() && !m.killed.get()
}

func (m *mockTransport) Features() transport.Features {
	return transport.Features{Reliable: true, Ordered: true, MessageOriented: true, MaxPayloadSize: 64 * 1024}
}

func (m *mockTransport) Reconnect(string) {
	// No-op: the mock never tears down/reestablishes on its own, tests
	// that need a path back drive it explicitly.
}

var _ transport.Transport = (*mockTransport)(nil)
