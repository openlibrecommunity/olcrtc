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
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/link"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
)

var (
	// ErrConnectFailed is returned when a tunnel connection fails.
	ErrConnectFailed = errors.New("tunnel connection failed")
	// ErrProxyAuth is returned when SOCKS proxy authentication fails.
	ErrProxyAuth = errors.New("SOCKS proxy auth failed")
	// ErrMuxExited is returned when the multiplexer loop exits unexpectedly.
	ErrMuxExited = errors.New("multiplexer loop exited")
	// ErrNoAvailableLinks is returned when no links are ready for sending.
	ErrNoAvailableLinks = errors.New("no available links")
)

// Client handles local SOCKS5 connections and tunnels them to the server.
type Client struct {
	links       []link.Link
	cipher      *crypto.Cipher
	mux         *mux.Multiplexer
	connections map[uint16]net.Conn
	connMu      sync.RWMutex
	clientID    uint32
	dnsServer   string
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
	videoWidth int,
	videoHeight int,
	videoFPS int,
	videoBitrate string,
	videoHW string,
	videoQRSize int,
) error {
	return RunWithReady(ctx, linkName, transportName, carrierName, roomURL, keyHex, localAddr, dnsServer, socksUser, socksPass, nil, videoWidth, videoHeight, videoFPS, videoBitrate, videoHW, videoQRSize)
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
	videoWidth int,
	videoHeight int,
	videoFPS int,
	videoBitrate string,
	videoHW string,
	videoQRSize int,
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
		if err := c.addLink(runCtx, linkName, transportName, carrierName, roomURL, i, cancel, dnsServer, "", 0, videoWidth, videoHeight, videoFPS, videoBitrate, videoHW, videoQRSize); err != nil {
			return fmt.Errorf("addLink failed: %w", err)
		}
	}

	lc := net.ListenConfig{}
	ln, err := lc.Listen(runCtx, "tcp4", localAddr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", localAddr, err)
	}
	defer ln.Close()

	logger.Infof("SOCKS5 server listening on %s (ClientID: %d)", localAddr, clientID)

	if onReady != nil {
		onReady()
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- c.acceptLoop(runCtx, ln)
	}()

	select {
	case <-runCtx.Done():
		c.shutdown()
		return nil
	case err := <-errCh:
		return err
	}
}

func (c *Client) shutdown() {
	c.connMu.Lock()
	for _, conn := range c.connections {
		if conn != nil {
			_ = conn.Close()
		}
	}
	c.connMu.Unlock()

	for i, ln := range c.links {
		logger.Infof("closing link %d", i)
		_ = ln.Close()
	}
}

func setupCipher(keyHex string) (*crypto.Cipher, error) {
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to decode key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}

	cipher, err := crypto.NewCipher(string(key))
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
			return err
		}
		if len(c.links) == 0 {
			return ErrNoAvailableLinks
		}
		return c.links[0].Send(encrypted)
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
	videoWidth, videoHeight, videoFPS int,
	videoBitrate, videoHW string,
	videoQRSize int,
) error {
	ln, err := link.New(ctx, linkName, link.Config{
		Transport:    transportName,
		Carrier:      carrierName,
		RoomURL:      roomURL,
		Name:         names.Generate(),
		OnData:       c.onData,
		DNSServer:    dnsServer,
		ProxyAddr:    socksProxyAddr,
		ProxyPort:    socksProxyPort,
		VideoWidth:   videoWidth,
		VideoHeight:  videoHeight,
		VideoFPS:     videoFPS,
		VideoBitrate: videoBitrate,
		VideoHW:      videoHW,
		VideoQRSize:  videoQRSize,
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

	if err := ln.Connect(ctx); err != nil {
		return fmt.Errorf("failed to connect link: %w", err)
	}

	go ln.WatchConnection(ctx)
	return nil
}

func (c *Client) handleLinkReconnect(linkID int) {
	logger.Infof("link %d reconnect event", linkID)
	c.sendResetSignal()

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
			return err
		}
		if len(c.links) == 0 {
			return ErrNoAvailableLinks
		}
		return c.links[0].Send(encrypted)
	})
	c.mux.Reset()
}

func (c *Client) sendResetSignal() {
	resetFrame := mux.BuildControlFrame(c.clientID, mux.ControlResetClient)
	encrypted, _ := c.cipher.Encrypt(resetFrame)
	if len(c.links) > 0 {
		_ = c.links[0].Send(encrypted)
	}
}

func (c *Client) acceptLoop(ctx context.Context, ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				logger.Warnf("Accept error: %v", err)
				continue
			}
		}
		go c.handleSocks5(ctx, conn)
	}
}

func (c *Client) handleSocks5(ctx context.Context, conn net.Conn) {
	defer conn.Close()

	if err := c.socks5Handshake(conn); err != nil {
		return
	}

	targetAddr, targetPort, err := c.socks5Request(conn)
	if err != nil {
		return
	}

	sid := c.mux.OpenStream()
	defer c.mux.CloseStream(sid)

	c.connMu.Lock()
	c.connections[sid] = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		delete(c.connections, sid)
		c.connMu.Unlock()
	}()

	logger.Infof("sid=%d tunnel to %s:%d", sid, targetAddr, targetPort)

	connectReq, _ := json.Marshal(map[string]any{
		"cmd":  "connect",
		"addr": targetAddr,
		"port": targetPort,
	})

	if err := c.mux.SendData(sid, connectReq); err != nil {
		logger.Warnf("sid=%d tunnel setup failed: %v", sid, err)
		_, _ = conn.Write(replyHostUnreachable())
		return
	}

	readyTimer := time.NewTimer(10 * time.Second)
	defer readyTimer.Stop()

	dataReady := c.mux.WaitForData(sid)

	var initialData []byte
	select {
	case <-readyTimer.C:
		logger.Warnf("sid=%d tunnel setup failed: timeout waiting for remote ready", sid)
		_, _ = conn.Write(replyHostUnreachable())
		return
	case <-dataReady:
		initialData = c.mux.ReadStream(sid)
		if len(initialData) == 0 || initialData[0] != 0x00 {
			logger.Warnf("sid=%d tunnel setup failed: invalid remote ready", sid)
			_, _ = conn.Write(replyHostUnreachable())
			return
		}
	}

	if _, err := conn.Write(replySuccess()); err != nil {
		return
	}

	// Handle the rest of initialData if any (unlikely for 0x00 packet)
	if len(initialData) > 1 {
		if _, err := conn.Write(initialData[1:]); err != nil {
			return
		}
	}

	go c.pumpFromMux(ctx, sid, conn)
	c.pumpToMux(sid, conn)
}

func (c *Client) socks5Handshake(conn net.Conn) error {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return err
	}
	if buf[0] != 5 {
		return fmt.Errorf("invalid socks version: %d", buf[0])
	}
	methods := make([]byte, buf[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return err
	}
	return nil
}

func (c *Client) socks5Request(conn net.Conn) (string, int, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}
	if header[1] != 1 {
		return "", 0, fmt.Errorf("unsupported socks command: %d", header[1])
	}

	var addr string
	switch header[3] {
	case 1: // IPv4
		buf := make([]byte, 4)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		addr = net.IP(buf).String()
	case 3: // Domain
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(conn, lenBuf); err != nil {
			return "", 0, err
		}
		buf := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		addr = string(buf)
	default:
		return "", 0, fmt.Errorf("unsupported address type: %d", header[3])
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBuf); err != nil {
		return "", 0, err
	}
	port := int(binary.BigEndian.Uint16(portBuf))

	return addr, port, nil
}

func (c *Client) pumpToMux(sid uint16, conn net.Conn) {
	buf := make([]byte, 16384)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return
		}

		for !c.canSendData() {
			time.Sleep(20 * time.Millisecond)
		}

		if err := c.mux.SendData(sid, buf[:n]); err != nil {
			return
		}
	}
}

func (c *Client) pumpFromMux(ctx context.Context, sid uint16, conn net.Conn) {
	defer c.mux.CleanupDataChannel(sid)
	dataReady := c.mux.WaitForData(sid)
	for {
		select {
		case <-ctx.Done():
			return
		case <-dataReady:
			data := c.mux.ReadStream(sid)
			if len(data) > 0 {
				if _, err := conn.Write(data); err != nil {
					return
				}
			}
			if c.mux.StreamClosed(sid) {
				return
			}
		}
	}
}

func (c *Client) onData(data []byte) {
	plaintext, err := c.cipher.Decrypt(data)
	if err != nil {
		return
	}
	c.mux.HandleFrame(plaintext)
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
