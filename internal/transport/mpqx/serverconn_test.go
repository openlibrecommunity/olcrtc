package mpqx_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"sync"
	"testing"
	"time"

	core "github.com/SolverNA/mpq-brutal/core"
	"github.com/openlibrecommunity/olcrtc/internal/transport/mpqx"
)

// peerHub emulates a peer-routing carrier (vp8channel/datachannel): several
// client ends share one logical server end. A client Send arrives at the server
// as OnPeerData(peerID, data); the server's SendTo(peerID, data) is delivered
// back to that one client's onData. This is the substrate the ServerPacketConn
// demuxes over, mirroring the production carrier.
type peerHub struct {
	mu      sync.Mutex
	clients map[string]*hubClient // peerID -> client end
	server  *mpqx.ServerPacketConn
}

func newPeerHub() *peerHub {
	return &peerHub{clients: make(map[string]*hubClient)}
}

// bindServer registers the server-side ServerPacketConn as the OnPeerData sink.
func (h *peerHub) bindServer(pc *mpqx.ServerPacketConn) {
	h.mu.Lock()
	h.server = pc
	h.mu.Unlock()
}

// SendTo implements mpqx.PeerSender: route a server message to one client.
func (h *peerHub) SendTo(peerID string, data []byte) error {
	h.mu.Lock()
	c := h.clients[peerID]
	h.mu.Unlock()
	if c == nil {
		return nil // unknown peer: drop like a lost datagram
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	go c.deliver(cp)
	return nil
}

// hubClient is one client end. It implements mpqx.Sender (Send -> server's
// OnPeerData tagged with this client's peerID) and forwards inbound to onData.
type hubClient struct {
	hub    *peerHub
	peerID string
	mu     sync.Mutex
	onData func([]byte)
}

func (h *peerHub) newClient(peerID string) *hubClient {
	c := &hubClient{hub: h, peerID: peerID}
	h.mu.Lock()
	h.clients[peerID] = c
	h.mu.Unlock()
	return c
}

func (c *hubClient) setOnData(cb func([]byte)) {
	c.mu.Lock()
	c.onData = cb
	c.mu.Unlock()
}

func (c *hubClient) deliver(data []byte) {
	c.mu.Lock()
	cb := c.onData
	c.mu.Unlock()
	if cb != nil {
		cb(data)
	}
}

// Send implements mpqx.Sender: deliver to the server tagged with our peerID.
func (c *hubClient) Send(data []byte) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	c.hub.mu.Lock()
	srv := c.hub.server
	c.hub.mu.Unlock()
	go func() {
		if srv != nil {
			srv.Deliver(c.peerID, cp)
		}
	}()
	return nil
}

// TestServerPacketConnMultiPathDemux stands up an mpq bonding session whose two
// client paths land on ONE peer-routing carrier and are demuxed by the
// ServerPacketConn into distinct QUIC connections, then regrouped by mpq into a
// single session. It pushes data both ways and asserts both paths carried it.
func TestServerPacketConnMultiPathDemux(t *testing.T) {
	hub := newPeerHub()
	srvPC := mpqx.NewServer(hub)
	hub.bindServer(srvPC)

	serverTLS, pin := generateTLSConfig(t)

	ln, err := core.Listen(core.Config{
		TLSConfig:     serverTLS,
		ServerConn:    srvPC,
		ExpectedPaths: 2,
		MaxPaths:      2,
	})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	srvCh := make(chan *core.Session, 1)
	accErr := make(chan error, 1)
	go func() {
		s, err := ln.Accept(ctx)
		if err != nil {
			accErr <- err
			return
		}
		srvCh <- s
	}()

	// Two client paths, each a distinct hub client (distinct peerID) wrapped in
	// its own 1:1 PacketConn — exactly how the tunnel client brings up N carriers.
	pathConns := make([]*mpqx.PacketConn, 2)
	for i := 0; i < 2; i++ {
		hc := hub.newClient(fmt.Sprintf("client-path-%d", i))
		pc := mpqx.New(hc)
		hc.setOnData(pc.Deliver)
		pathConns[i] = pc
	}

	dialPath := func(_ context.Context, pathIndex int, _ string) (net.PacketConn, net.Addr, error) {
		return pathConns[pathIndex], mpqx.RemoteAddr(), nil
	}
	client, err := core.Dial(ctx, core.Config{
		DialPath:      dialPath,
		MaxPaths:      2,
		ExpectedPaths: 2,
		TLSConfig:     core.PinnedClientTLS("localhost", pin, nil),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	// Second path on the fly (pathIndex 1).
	if _, err := client.AddPath(ctx, ""); err != nil {
		t.Fatalf("AddPath: %v", err)
	}

	var server *core.Session
	select {
	case server = <-srvCh:
	case err := <-accErr:
		t.Fatalf("Accept: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for Accept")
	}
	defer server.Close()

	if got := len(client.Paths()); got != 2 {
		t.Fatalf("client has %d paths, want 2", got)
	}
	if got := len(server.Paths()); got != 2 {
		t.Fatalf("server has %d paths, want 2", got)
	}

	const total = 1 << 20 // 1 MB each way
	up := make([]byte, total)
	mrand.New(mrand.NewSource(1)).Read(up)
	upErr := make(chan error, 1)
	go func() {
		_, err := client.Write(up)
		upErr <- err
	}()
	upRecv := make([]byte, total)
	if _, err := io.ReadFull(server, upRecv); err != nil {
		t.Fatalf("server read: %v", err)
	}
	if err := <-upErr; err != nil {
		t.Fatalf("client write: %v", err)
	}
	if !bytes.Equal(up, upRecv) {
		t.Fatal("upstream mismatch")
	}

	down := make([]byte, total)
	mrand.New(mrand.NewSource(2)).Read(down)
	downErr := make(chan error, 1)
	go func() {
		_, err := server.Write(down)
		downErr <- err
	}()
	downRecv := make([]byte, total)
	if _, err := io.ReadFull(client, downRecv); err != nil {
		t.Fatalf("client read: %v", err)
	}
	if err := <-downErr; err != nil {
		t.Fatalf("server write: %v", err)
	}
	if !bytes.Equal(down, downRecv) {
		t.Fatal("downstream mismatch")
	}

	for _, p := range server.Paths() {
		if p.RecvBytes() == 0 {
			t.Errorf("server path %d received no bytes (demux collapsed paths?)", p.ID())
		}
	}
}
