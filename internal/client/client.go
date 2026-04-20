// Package client implements the local SOCKS5 client side of the olcrtc tunnel.
package client

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/link"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
)

var (
	// ErrKeySize is returned when the key size is not 32 bytes.
	ErrKeySize = errors.New("key must be 32 bytes")
	// ErrKeyStringLength is returned when the key string length is not 32.
	ErrKeyStringLength = errors.New("key string length must be 32")
	// ErrInvalidSocks5 is returned when the SOCKS version is not 5.
	ErrInvalidSocks5 = errors.New("invalid SOCKS5 version")
	// ErrNoLinks is returned when no links are available for sending.
	ErrNoLinks = errors.New("no links available")
	// ErrEncryptFailed is returned when encryption fails.
	ErrEncryptFailed = errors.New("encrypt failed")
	// ErrUnsupportedSocksCommand is returned when a SOCKS5 command is not supported.
	ErrUnsupportedSocksCommand = errors.New("unsupported SOCKS5 command")
	// ErrUnsupportedAddressType is returned when a SOCKS5 address type is not supported.
	ErrUnsupportedAddressType = errors.New("unsupported address type")
	// ErrTunnelSetupFailed is returned when the tunnel cannot be established.
	ErrTunnelSetupFailed = errors.New("tunnel setup failed")
)

// Client handles local SOCKS5 connections and tunnels them through the selected runtime stack.
type Client struct {
	links         []link.Link
	cipher        *crypto.Cipher
	mux           *mux.Multiplexer
	connections   map[uint16]net.Conn
	connMu        sync.RWMutex
	linkIdx       atomic.Uint32
	clientID      uint32
	activeClients atomic.Int32
	wg            sync.WaitGroup
	dnsServer     string
}

// Run starts the client with the specified parameters.
func Run(
	ctx context.Context,
	linkName,
	transportName,
	carrierName,
	roomURL,
	keyHex string,
	localAddr string,
	dnsServer,
	socksUser string,
	socksPass string,
) error {
	return RunWithReady(ctx, linkName, transportName, carrierName, roomURL, keyHex, localAddr, dnsServer, socksUser, socksPass, nil)
}

// RunWithReady is like Run but accepts a callback that is called when the client is ready.
func RunWithReady(
	ctx context.Context,
	linkName,
	transportName,
	carrierName,
	roomURL,
	keyHex string,
	localAddr string,
	dnsServer,
	_ string,
	_ string,
	onReady func(),
) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cipher, err := setupCipher(keyHex)
	if err != nil {
		return fmt.Errorf("setupCipher failed: %w", err)
	}

	clientIDBytes := make([]byte, 4)
	if _, err := rand.Read(clientIDBytes); err != nil {
		return fmt.Errorf("failed to generate client ID: %w", err)
	}
	clientID := binary.BigEndian.Uint32(clientIDBytes)

	c := &Client{
		cipher:      cipher,
		connections: make(map[uint16]net.Conn),
		links:       make([]link.Link, 0),
		clientID:    clientID,
		dnsServer:   dnsServer,
	}

	c.setupMux()

	const linkCount = 1
	for i := range linkCount {
		if err := c.addLink(runCtx, linkName, transportName, carrierName, roomURL, i, cancel, dnsServer, "", 0); err != nil {
			return fmt.Errorf("addLink failed: %w", err)
		}
	}

	lc := net.ListenConfig{}
	ln, err := lc.Listen(runCtx, "tcp", localAddr)
	if err != nil {
		return fmt.Errorf("listen failed: %w", err)
	}
	defer func() { _ = ln.Close() }()

	logger.Infof("SOCKS5 server listening on %s (ClientID: %d)", localAddr, clientID)

	if onReady != nil {
		onReady()
	}

	go c.acceptLoop(runCtx, ln)

	<-runCtx.Done()
	c.shutdown()
	c.wg.Wait()

	return nil
}

func setupCipher(keyHex string) (*crypto.Cipher, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, ErrKeySize
	}

	keyStr := string(key)
	if len(keyStr) != 32 {
		return nil, ErrKeyStringLength
	}

	cipher, err := crypto.NewCipher(keyStr)
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %w", err)
	}
	return cipher, nil
}

func (c *Client) setupMux() {
	c.mux = mux.New(c.clientID, func(frame []byte) error {
		for {
			canSend := true
			for _, ln := range c.links {
				if !ln.CanSend() {
					canSend = false
					break
				}
			}
			if canSend {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}

		encrypted, err := c.cipher.Encrypt(frame)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrEncryptFailed, err)
		}
		if len(c.links) == 0 {
			return ErrNoLinks
		}
		idx := c.linkIdx.Add(1) % uint32(len(c.links)) //nolint:gosec
		return c.links[idx].Send(encrypted)
	})
}

func (c *Client) addLink(
	ctx context.Context,
	linkName,
	transportName,
	carrierName,
	roomURL string,
	linkID int,
	cancel context.CancelFunc,
	dnsServer,
	socksProxyAddr string,
	socksProxyPort int,
) error {
	ln, err := link.New(ctx, linkName, link.Config{
		Transport: transportName,
		Carrier:   carrierName,
		RoomURL:   roomURL,
		Name:      names.Generate(),
		OnData:    c.onData,
		DNSServer: dnsServer,
		ProxyAddr: socksProxyAddr,
		ProxyPort: socksProxyPort,
	})
	if err != nil {
		return fmt.Errorf("failed to create link: %w", err)
	}

	ln.SetEndedCallback(func(reason string) {
		logger.Infof("Client link %d reported conference end: %s", linkID, reason)
		cancel()
	})
	c.links = append(c.links, ln)

	ln.SetReconnectCallback(func() {
		c.handleLinkReconnect(linkID)
	})

	logger.Infof("Connecting link %d via %s/%s/%s...", linkID, linkName, transportName, carrierName)
	if err := ln.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect link: %w", err)
	}
	logger.Infof("Link %d connected", linkID)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		ln.WatchConnection(ctx)
	}()

	// Send initial reset to clean up any stale connections for this clientID on server
	if err := c.mux.SendClientReset(); err != nil {
		logger.Warnf("Failed to send initial client reset: %v", err)
	}

	return nil
}

func (c *Client) handleLinkReconnect(linkID int) {
	logger.Infof("link %d reconnect event", linkID)

	c.connMu.Lock()
	for sid, conn := range c.connections {
		if conn != nil {
			_ = conn.Close()
		}
		delete(c.connections, sid)
	}
	c.connMu.Unlock()

	c.mux.UpdateSendFunc(func(frame []byte) error {
		encrypted, err := c.cipher.Encrypt(frame)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrEncryptFailed, err)
		}
		if len(c.links) == 0 {
			return ErrNoLinks
		}
		idx := c.linkIdx.Add(1) % uint32(len(c.links)) //nolint:gosec
		return c.links[idx].Send(encrypted)
	})
	c.mux.Reset()

	if err := c.mux.SendClientReset(); err != nil {
		logger.Warnf("Failed to send client reset after reconnect: %v", err)
	}
}

func (c *Client) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
			conn, err := ln.Accept()
			if err != nil {
				logger.Debugf("Accept error: %v", err)
				continue
			}
			go c.handleSOCKS5(ctx, conn)
		}
	}
}

func (c *Client) handleSOCKS5(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	if err := c.socks5Handshake(conn); err != nil {
		logger.Debugf("SOCKS5 handshake failed: %v", err)
		return
	}

	addr, port, err := c.socks5Request(conn)
	if err != nil {
		logger.Debugf("SOCKS5 request failed: %v", err)
		return
	}

	sid := c.mux.OpenStream()
	c.connMu.Lock()
	c.connections[sid] = conn
	c.connMu.Unlock()

	logger.Infof("sid=%d tunnel to %s:%d", sid, addr, port)

	if err := c.setupTunnel(ctx, sid, conn, addr, port); err != nil {
		logger.Warnf("sid=%d tunnel setup failed: %v", sid, err)
		return
	}

	c.activeClients.Add(1)
	c.startStreamPump(ctx, sid, conn)
	c.pumpToMux(sid, conn)
}

func (c *Client) setupTunnel(ctx context.Context, sid uint16, conn net.Conn, addr string, port int) error {
	req := map[string]any{"cmd": "connect", "addr": addr, "port": port}
	reqData, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal connect: %w", err)
	}

	if err := c.mux.SendData(sid, reqData); err != nil {
		return fmt.Errorf("send connect: %w", err)
	}

	dataReady := c.mux.WaitForData(sid)
	select {
	case <-dataReady:
		resp := c.mux.ReadStream(sid)
		if len(resp) > 0 && resp[0] == 0x00 {
			if _, err := conn.Write(replySuccess()); err != nil {
				return fmt.Errorf("write success: %w", err)
			}
		} else {
			_, _ = conn.Write(replyHostUnreachable())
			return ErrTunnelSetupFailed
		}
	case <-time.After(15 * time.Second):
		_, _ = conn.Write(replyHostUnreachable())
		c.mux.CleanupDataChannel(sid)
		return fmt.Errorf("%w: timeout", ErrTunnelSetupFailed)
	case <-ctx.Done():
		return fmt.Errorf("context cancelled: %w", ctx.Err())
	}
	c.mux.CleanupDataChannel(sid)
	return nil
}

func (c *Client) socks5Handshake(conn net.Conn) error {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	if buf[0] != 5 {
		return ErrInvalidSocks5
	}

	methods := make([]byte, int(buf[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return fmt.Errorf("read methods: %w", err)
	}

	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}

func (c *Client) socks5Request(conn net.Conn) (string, int, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return "", 0, fmt.Errorf("read request header: %w", err)
	}

	if buf[0] != 5 || buf[1] != 1 {
		return "", 0, fmt.Errorf("%w: cmd=%d", ErrUnsupportedSocksCommand, buf[1])
	}

	addr, err := c.readSocks5Addr(conn, buf[3])
	if err != nil {
		return "", 0, err
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", 0, fmt.Errorf("read port: %w", err)
	}
	port := int(binary.BigEndian.Uint16(portBuf))

	return addr, port, nil
}

func (c *Client) readSocks5Addr(conn net.Conn, addrType byte) (string, error) {
	switch addrType {
	case 1: // IPv4
		ip := make([]byte, 4)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", fmt.Errorf("read ipv4: %w", err)
		}
		return net.IP(ip).String(), nil
	case 3: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", fmt.Errorf("read domain len: %w", err)
		}
		domain := make([]byte, int(lenBuf[0]))
		if _, err := io.ReadFull(conn, domain); err != nil {
			return "", fmt.Errorf("read domain: %w", err)
		}
		return string(domain), nil
	case 4: // IPv6
		ip := make([]byte, 16)
		if _, err := io.ReadFull(conn, ip); err != nil {
			return "", fmt.Errorf("read ipv6: %w", err)
		}
		return net.IP(ip).String(), nil
	default:
		return "", fmt.Errorf("%w: type=%d", ErrUnsupportedAddressType, addrType)
	}
}

func (c *Client) onData(data []byte) {
	plaintext, err := c.cipher.Decrypt(data)
	if err != nil {
		logger.Debugf("Decrypt error: %v", err)
		return
	}

	c.mux.HandleFrame(plaintext)
}

func (c *Client) shutdown() {
	c.connMu.Lock()
	for _, conn := range c.connections {
		if conn != nil {
			_ = conn.Close()
		}
	}
	c.connMu.Unlock()

	for i, tr := range c.links {
		logger.Infof("closing link %d", i)
		_ = tr.Close()
	}
}

func (c *Client) pumpToMux(sid uint16, conn net.Conn) {
	defer func() {
		c.activeClients.Add(-1)
		_ = c.mux.CloseStream(sid)
		c.connMu.Lock()
		delete(c.connections, sid)
		c.connMu.Unlock()
	}()

	buf := make([]byte, 16384)
	totalSent := uint64(0)
	lastLog := time.Now()

	for {
		n, err := conn.Read(buf)
		if err != nil {
			if totalSent > 1024*1024 {
				logger.Infof("sid=%d done total=%dMB", sid, totalSent/(1024*1024))
			}
			return
		}

		for !c.canSendData() {
			time.Sleep(20 * time.Millisecond)
		}

		if err := c.mux.SendData(sid, buf[:n]); err != nil {
			return
		}

		totalSent += uint64(n) //nolint:gosec
		if time.Since(lastLog) > 5*time.Second {
			logger.Infof("sid=%d sent=%dMB", sid, totalSent/(1024*1024))
			lastLog = time.Now()
		}
	}
}

func (c *Client) startStreamPump(ctx context.Context, sid uint16, conn net.Conn) {
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				data := c.mux.ReadStream(sid)
				if len(data) > 0 {
					if _, err := conn.Write(data); err != nil {
						_ = c.mux.CloseStream(sid)
						return
					}
				}
				if c.mux.StreamClosed(sid) {
					return
				}
			}
		}
	}()
}

func (c *Client) canSendData() bool {
	for _, tr := range c.links {
		if !tr.CanSend() {
			return false
		}
	}
	return true
}

func replySuccess() []byte {
	return []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
}

func replyHostUnreachable() []byte {
	return []byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}
}
