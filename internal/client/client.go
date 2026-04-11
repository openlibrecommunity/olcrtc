//nolint:revive
package client

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/errors"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
	"github.com/pion/webrtc/v4"
)

//nolint:revive
type Client struct {
	peers    []*telemost.Peer
	cipher   *crypto.Cipher
	mux      *mux.Multiplexer
	clientID uint32
	peerIdx  atomic.Uint32
	wg       sync.WaitGroup
}

//nolint:revive
func Run(ctx context.Context, roomURL, keyHex string, socksPort int, duo bool) error {
	key, cipher, err := initCipher(keyHex)
	if err != nil {
		return err
	}

	clientID := uint32(time.Now().UnixNano() & 0xFFFFFFFF)

	c := &Client{
		cipher:   cipher,
		clientID: clientID,
		peers:    make([]*telemost.Peer, 0),
	}

	peerCount := 1
	if duo {
		peerCount = 2
		log.Println("Duo mode: using 2 parallel channels")
	}

	c.mux = mux.New(c.clientID, c.createSendFunc())

	if err := c.connectPeers(ctx, roomURL, peerCount); err != nil {
		return err
	}

	time.Sleep(100 * time.Millisecond)
	c.sendResetSignal(key)

	err = c.runSOCKS5(ctx, socksPort)

	log.Println("Waiting for client goroutines...")
	c.wg.Wait()
	log.Println("Client goroutines finished")

	return err
}

func initCipher(keyHex string) ([]byte, *crypto.Cipher, error) {
	var key []byte
	var err error

	if keyHex == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, nil, fmt.Errorf("failed to generate key: %w", err)
		}
		log.Printf("Generated key: %x", key)
	} else {
		key, err = hex.DecodeString(keyHex)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to decode key: %w", err)
		}
		if len(key) != 32 {
			return nil, nil, errors.KeySizeError{Got: len(key)}
		}
	}

	keyStr := string(key)
	if len(keyStr) != 32 {
		return nil, nil, errors.KeyStringLengthError{Got: len(keyStr)}
	}

	cipher, err := crypto.NewCipher(keyStr)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create cipher: %w", err)
	}

	return key, cipher, nil
}

func (c *Client) createSendFunc() func([]byte) error {
	return func(frame []byte) error {
		for {
			canSend := true
			for _, peer := range c.peers {
				if !peer.CanSend() {
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
			return fmt.Errorf("failed to encrypt frame: %w", err)
		}

		if len(c.peers) == 0 {
			return errors.ErrNoPeersAvailable
		}
		if len(c.peers) > int(^uint32(0)) {
			return errors.ErrTooManyPeers
		}
		//nolint:gosec
		peerCount := uint32(len(c.peers))
		idx := c.peerIdx.Add(1) % peerCount
		return c.peers[idx].Send(encrypted)
	}
}

func (c *Client) connectPeers(ctx context.Context, roomURL string, peerCount int) error {
	for i := range peerCount {
		peer, err := telemost.NewPeer(ctx, roomURL, names.Generate(), c.onData)
		if err != nil {
			return fmt.Errorf("failed to create peer: %w", err)
		}
		c.peers = append(c.peers, peer)

		peer.SetReconnectCallback(func(_ *webrtc.DataChannel) {
			log.Printf("Client peer %d reconnected - resetting multiplexer state", i)
			c.mux.UpdateSendFunc(c.createSendFunc())
			c.mux.Reset()
			log.Println("Client multiplexer reset complete")
		})

		log.Printf("Connecting peer %d to Telemost...", i)
		if err := peer.Connect(ctx); err != nil {
			return fmt.Errorf("failed to connect peer: %w", err)
		}
		log.Printf("Peer %d connected", i)

		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			peer.WatchConnection(ctx)
		}()
	}
	return nil
}

func (c *Client) sendResetSignal(_ []byte) {
	resetFrame := make([]byte, 12)
	binary.BigEndian.PutUint32(resetFrame[0:4], c.clientID)
	binary.BigEndian.PutUint16(resetFrame[4:6], 0xFFFF)
	binary.BigEndian.PutUint16(resetFrame[6:8], 0xFFFF)
	binary.BigEndian.PutUint32(resetFrame[8:12], 0)
	encrypted, _ := c.cipher.Encrypt(resetFrame)

	for _, peer := range c.peers {
		if err := peer.Send(encrypted); err != nil {
			log.Printf("Failed to send reset signal: %v", err)
		}
	}
	log.Printf("Sent reset signal to server (clientID=%d)", c.clientID)
}

func (c *Client) onData(data []byte) {
	plaintext, err := c.cipher.Decrypt(data)
	if err != nil {
		logger.Debug("Decrypt error: %v", err)
		return
	}

	c.mux.HandleFrame(plaintext)
}
func (c *Client) runSOCKS5(ctx context.Context, port int) error {
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return fmt.Errorf("failed to listen: %w", err)
	}

	log.Printf("SOCKS5 proxy listening on 0.0.0.0:%d", port)

	go func() {
		<-ctx.Done()
		log.Println("Closing SOCKS5 listener...")
		if err := listener.Close(); err != nil {
			log.Printf("Error closing listener: %v", err)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Println("SOCKS5 listener closed")

				for _, peer := range c.peers {
					if err := peer.Close(); err != nil {
						log.Printf("Error closing peer: %v", err)
					}
				}

				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}

		go c.handleSOCKS5(conn)
	}
}

func (c *Client) handleSOCKS5(conn net.Conn) {
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Error closing connection: %v", err)
		}
	}()

	buf := make([]byte, 256)

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}

	if buf[0] != 5 {
		return
	}

	nmethods := buf[1]
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return
	}

	if _, err := conn.Write([]byte{5, 0}); err != nil {
		log.Printf("Error writing SOCKS5 response: %v", err)
		return
	}

	addr, port, ok := c.readSOCKS5Request(conn, buf)
	if !ok {
		return
	}

	sid := c.mux.OpenStream()
	logger.Verbose("SOCKS5 opened stream sid=%d for %s:%d", sid, addr, port)
	log.Printf("[CLIENT] sid=%d SOCKS5_START %s:%d", sid, addr, port)

	if !c.sendConnectRequest(sid, addr, port) {
		return
	}

	if !c.waitForConnection(sid, conn) {
		return
	}

	c.mux.ReadStream(sid)

	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		log.Printf("Error writing success response: %v", err)
		return
	}

	c.streamData(sid, conn)
}

func (c *Client) readSOCKS5Request(conn net.Conn, buf []byte) (string, uint16, bool) {
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return "", 0, false
	}

	if buf[1] != 1 {
		c.writeSOCKS5Error(conn, 7)
		return "", 0, false
	}

	addr, ok := c.readSOCKS5Address(conn, buf, buf[3])
	if !ok {
		return "", 0, false
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", 0, false
	}
	port := binary.BigEndian.Uint16(buf[:2])

	return addr, port, true
}

func (c *Client) readSOCKS5Address(conn net.Conn, buf []byte, atyp byte) (string, bool) {
	switch atyp {
	case 1:
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return "", false
		}
		return fmt.Sprintf("%d.%d.%d.%d", buf[0], buf[1], buf[2], buf[3]), true
	case 3:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return "", false
		}
		length := buf[0]
		if _, err := io.ReadFull(conn, buf[:length]); err != nil {
			return "", false
		}
		return string(buf[:length]), true
	default:
		c.writeSOCKS5Error(conn, 8)
		return "", false
	}
}

func (c *Client) writeSOCKS5Error(conn net.Conn, code byte) {
	if _, err := conn.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		log.Printf("Error writing SOCKS5 error response: %v", err)
	}
}

func (c *Client) sendConnectRequest(sid uint16, addr string, port uint16) bool {
	req := map[string]any{
		"cmd":  "connect",
		"addr": addr,
		"port": port,
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		log.Printf("Error marshaling connect request: %v", err)
		return false
	}

	if err := c.mux.SendData(sid, reqData); err != nil {
		log.Printf("Error sending connect request: %v", err)
		return false
	}
	return true
}

func (c *Client) waitForConnection(sid uint16, conn net.Conn) bool {
	dataReady := c.mux.WaitForData(sid)
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	select {
	case <-dataReady:
		stream := c.mux.GetStream(sid)
		if stream == nil || len(stream.RecvBuf()) == 0 {
			if _, err := conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
				log.Printf("Error writing timeout response: %v", err)
			}
			return false
		}
	case <-timeout.C:
		if _, err := conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			log.Printf("Error writing timeout response: %v", err)
		}
		return false
	}
	return true
}

func (c *Client) streamData(sid uint16, conn net.Conn) {
	done := make(chan struct{})
	streamClosed := make(chan struct{})

	go c.readFromConnection(sid, conn, done)
	go c.writeToConnection(sid, conn, done, streamClosed)

	select {
	case <-done:
	case <-streamClosed:
	}
}

func (c *Client) readFromConnection(sid uint16, conn net.Conn, done chan struct{}) {
	defer close(done)
	buf := make([]byte, 32768)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err := c.mux.CloseStream(sid); err != nil {
				log.Printf("Error closing stream: %v", err)
			}
			return
		}
		if err := c.mux.SendData(sid, buf[:n]); err != nil {
			return
		}
	}
}

func (c *Client) writeToConnection(sid uint16, conn net.Conn, done chan struct{}, streamClosed chan struct{}) {
	defer close(streamClosed)
	defer c.mux.CleanupDataChannel(sid)

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			c.writeStreamData(sid, conn)
			if c.mux.StreamClosed(sid) {
				return
			}
		}
	}
}

func (c *Client) writeStreamData(sid uint16, conn net.Conn) {
	data := c.mux.ReadStream(sid)
	if len(data) == 0 {
		return
	}
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return
		}
		data = data[n:]
	}
}
