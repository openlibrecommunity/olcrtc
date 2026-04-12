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
	"log"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
	"github.com/pion/webrtc/v4"
)

var (
	errInvalidKeyLength       = errors.New("key must be 32 bytes")
	errInvalidKeyStringLength = errors.New("key string length must be 32")
	errNoConnectedPeers       = errors.New("no connected peers available")
)

// Client manages the client-side mux and SOCKS5 listener.
type Client struct {
	peers    []*telemost.Peer
	cipher   *crypto.Cipher
	mux      *mux.Multiplexer
	clientID uint32
	peerIdx  atomic.Uint32
	wg       sync.WaitGroup
}

const defaultSOCKSListenHost = "127.0.0.1"

// Run starts the client and listens for SOCKS5 traffic.
func Run(
	ctx context.Context,
	roomURL,
	keyHex string,
	socksPort int,
	socksHost,
	socksUser,
	socksPass string,
) error {
	return RunWithReady(ctx, roomURL, keyHex, socksPort, socksHost, socksUser, socksPass, nil)
}

// RunWithReady starts the client and invokes onReady once the local SOCKS5 listener is accepting connections.
func RunWithReady(
	ctx context.Context,
	roomURL,
	keyHex string,
	socksPort int,
	socksHost,
	socksUser,
	socksPass string,
	onReady func(),
) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	key, err := decodeKey(keyHex)
	if err != nil {
		return fmt.Errorf("decodeKey failed: %w", err)
	}

	keyStr := string(key)
	if len(keyStr) != 32 {
		return fmt.Errorf("%w: got %d", errInvalidKeyStringLength, len(keyStr))
	}

	cipher, err := crypto.NewCipher(keyStr)
	if err != nil {
		return fmt.Errorf("create cipher: %w", err)
	}

	c := &Client{
		cipher:   cipher,
		clientID: uint32(time.Now().UnixNano() & 0xFFFFFFFF),
		peers:    make([]*telemost.Peer, 0, 1),
	}

	c.mux = mux.New(c.clientID, c.sendFrame)

	for peerID := range 1 {
		if err := c.addPeer(runCtx, roomURL, peerID, cancel); err != nil {
			return fmt.Errorf("addPeer failed: %w", err)
		}
	}

	time.Sleep(100 * time.Millisecond)
	c.sendResetSignal()

	err = c.runSOCKS5(runCtx, socksHost, socksPort, socksUser, socksPass, onReady)

	log.Println("Waiting for client goroutines...")
	c.wg.Wait()
	log.Println("Client goroutines finished")

	return err
}

func decodeKey(keyHex string) ([]byte, error) {
	if keyHex == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate random key: %w", err)
		}

		log.Printf("Generated key: %x", key)
		return key, nil
	}

	key, err := hex.DecodeString(keyHex)
	if err != nil {
		return nil, fmt.Errorf("decode hex key: %w", err)
	}

	if len(key) != 32 {
		return nil, fmt.Errorf("%w: got %d", errInvalidKeyLength, len(key))
	}

	return key, nil
}

func (c *Client) sendFrame(frame []byte) error {
	waitUntilPeersCanSend(c.peers)

	encrypted, err := c.cipher.Encrypt(frame)
	if err != nil {
		return fmt.Errorf("encrypt outgoing frame: %w", err)
	}

	peer, err := c.nextPeer()
	if err != nil {
		return err
	}

	if err := peer.Send(encrypted); err != nil {
		return fmt.Errorf("send frame via peer: %w", err)
	}

	return nil
}

func waitUntilPeersCanSend(peers []*telemost.Peer) {
	for {
		canSend := true
		for _, peer := range peers {
			if !peer.CanSend() {
				canSend = false
				break
			}
		}

		if canSend {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (c *Client) nextPeer() (*telemost.Peer, error) {
	switch len(c.peers) {
	case 0:
		return nil, errNoConnectedPeers
	case 1:
		return c.peers[0], nil
	default:
		return c.peers[int(c.peerIdx.Add(1)%2)], nil
	}
}

func (c *Client) addPeer(
	runCtx context.Context,
	roomURL string,
	peerID int,
	cancel context.CancelFunc,
) error {
	peer, err := telemost.NewPeer(runCtx, roomURL, names.Generate(), c.onData)
	if err != nil {
		return fmt.Errorf("create peer %d: %w", peerID, err)
	}

	peer.SetEndedCallback(func(reason string) {
		log.Printf("Client peer %d reported conference end: %s", peerID, reason)
		cancel()
	})

	peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
		c.onReconnect(peerID, dc)
	})

	c.peers = append(c.peers, peer)

	log.Printf("Connecting peer %d to Telemost...", peerID)
	if err := peer.Connect(runCtx); err != nil {
		return fmt.Errorf("connect peer %d: %w", peerID, err)
	}
	log.Printf("Peer %d connected", peerID)

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		peer.WatchConnection(runCtx)
	}()

	return nil
}

func (c *Client) onReconnect(peerID int, dc *webrtc.DataChannel) {
	if dc == nil {
		log.Printf("Client peer %d channel closed - resetting multiplexer state", peerID)
	} else {
		log.Printf("Client peer %d reconnected - resetting multiplexer state", peerID)
	}

	c.mux.UpdateSendFunc(c.sendFrame)
	c.mux.Reset()

	log.Println("Client multiplexer reset complete")
}

func (c *Client) sendResetSignal() {
	resetFrame := mux.BuildControlFrame(c.clientID, mux.ControlResetClient)
	encrypted, err := c.cipher.Encrypt(resetFrame)
	if err != nil {
		log.Printf("Failed to encrypt reset signal: %v", err)
		return
	}

	for _, peer := range c.peers {
		if err := peer.Send(encrypted); err != nil {
			log.Printf("Failed to send reset signal to server: %v", err)
		}
	}

	log.Printf("Sent reset signal to server (clientID=%d)", c.clientID)
}

func (c *Client) onData(data []byte) {
	plaintext, err := c.cipher.Decrypt(data)
	if err != nil {
		logger.Debugf("Decrypt error: %v", err)
		return
	}

	c.mux.HandleFrame(plaintext)
}

func (c *Client) runSOCKS5(
	ctx context.Context,
	host string,
	port int,
	username,
	password string,
	onReady func(),
) error {
	if host == "" {
		host = defaultSOCKSListenHost
	}

	listenAddr := net.JoinHostPort(host, strconv.Itoa(port))
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listenAddr, err)
	}

	log.Printf("SOCKS5 proxy listening on %s (auth=%v)", listenAddr, username != "")
	if onReady != nil {
		onReady()
	}

	go func() {
		<-ctx.Done()
		log.Println("Closing SOCKS5 listener...")
		if err := listener.Close(); err != nil {
			logger.Debugf("SOCKS5 listener close error: %v", err)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				log.Println("SOCKS5 listener closed")
				c.closePeers()
				return nil
			default:
				log.Printf("Accept error: %v", err)
				continue
			}
		}

		go c.handleSOCKS5(conn, username, password)
	}
}

func (c *Client) closePeers() {
	for _, peer := range c.peers {
		if err := peer.Close(); err != nil {
			logger.Debugf("Peer close error: %v", err)
		}
	}
}

//nolint:cyclop // SOCKS5 parsing is inherently stateful and mirrors the protocol handshake.
func (c *Client) handleSOCKS5(conn net.Conn, username, password string) {
	defer func() {
		if err := conn.Close(); err != nil {
			logger.Debugf("SOCKS5 connection close error: %v", err)
		}
	}()

	buf := make([]byte, 513)
	if !readSOCKSVersionAndMethods(conn, buf) {
		return
	}

	nmethods := buf[1]
	if _, err := io.ReadFull(conn, buf[:nmethods]); err != nil {
		return
	}

	requireAuth := username != ""
	wantMethod := byte(0x00)
	if requireAuth {
		wantMethod = 0x02
	}

	if !supportsMethod(buf[:nmethods], wantMethod) {
		writeResponse(conn, replyUnsupportedSOCKSMethod())
		return
	}
	writeResponse(conn, []byte{5, wantMethod})

	if requireAuth && !authenticateSOCKSUser(conn, buf, username, password) {
		return
	}

	addr, port, ok := readConnectTarget(conn, buf)
	if !ok {
		return
	}

	sid := c.mux.OpenStream()
	logger.Verbosef("SOCKS5 opened stream sid=%d for %s:%d", sid, addr, port)
	log.Printf("[CLIENT] sid=%d SOCKS5_START %s:%d", sid, addr, port)

	if !c.sendConnectRequest(sid, addr, port) {
		return
	}

	if !c.waitConnectResponse(conn, sid) {
		return
	}

	c.mux.ReadStream(sid)
	writeResponse(conn, replySuccess())
	c.proxyStream(conn, sid)
}

func readSOCKSVersionAndMethods(conn net.Conn, buf []byte) bool {
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return false
	}

	return buf[0] == 5
}

func supportsMethod(methods []byte, wantMethod byte) bool {
	for _, method := range methods {
		if method == wantMethod {
			return true
		}
	}

	return false
}

func authenticateSOCKSUser(conn net.Conn, buf []byte, username, password string) bool {
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return false
	}
	if buf[0] != 0x01 {
		return false
	}

	ulen := int(buf[1])
	if _, err := io.ReadFull(conn, buf[:ulen+1]); err != nil {
		return false
	}

	gotUser := string(buf[:ulen])
	plen := int(buf[ulen])
	if _, err := io.ReadFull(conn, buf[:plen]); err != nil {
		return false
	}

	gotPass := string(buf[:plen])
	if gotUser != username || gotPass != password {
		writeResponse(conn, replyAuthFailed())
		return false
	}

	writeResponse(conn, replyAuthOK())
	return true
}

func readConnectTarget(conn net.Conn, buf []byte) (string, uint16, bool) {
	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return "", 0, false
	}

	if buf[1] != 1 {
		writeResponse(conn, replyCommandNotSupported())
		return "", 0, false
	}

	addr, ok := readTargetAddress(conn, buf, buf[3])
	if !ok {
		return "", 0, false
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return "", 0, false
	}

	return addr, binary.BigEndian.Uint16(buf[:2]), true
}

func readTargetAddress(conn net.Conn, buf []byte, atyp byte) (string, bool) {
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
		writeResponse(conn, replyAddressNotSupported())
		return "", false
	}
}

func (c *Client) sendConnectRequest(sid uint16, addr string, port uint16) bool {
	reqData, err := json.Marshal(struct {
		Cmd  string `json:"cmd"`
		Addr string `json:"addr"`
		Port uint16 `json:"port"`
	}{
		Cmd:  "connect",
		Addr: addr,
		Port: port,
	})
	if err != nil {
		logger.Debugf("Connect request marshal error: %v", err)
		return false
	}

	if err := c.mux.SendData(sid, reqData); err != nil {
		logger.Debugf("Connect request send error: %v", err)
		return false
	}

	return true
}

func (c *Client) waitConnectResponse(conn net.Conn, sid uint16) bool {
	dataReady := c.mux.WaitForData(sid)
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	select {
	case <-dataReady:
		stream := c.mux.GetStream(sid)
		if stream == nil || len(stream.RecvBuf()) == 0 {
			writeResponse(conn, replyHostUnreachable())
			return false
		}
	case <-timeout.C:
		writeResponse(conn, replyHostUnreachable())
		return false
	}

	return true
}

//nolint:cyclop // The stream pump handles two coordinated goroutines and shutdown races in one place.
func (c *Client) proxyStream(conn net.Conn, sid uint16) {
	done := make(chan struct{})
	streamClosed := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				if err := c.mux.CloseStream(sid); err != nil {
					logger.Debugf("Close stream error: %v", err)
				}
				return
			}
			if err := c.mux.SendData(sid, buf[:n]); err != nil {
				return
			}
		}
	}()

	go func() {
		defer close(streamClosed)
		defer c.mux.CleanupDataChannel(sid)

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				data := c.mux.ReadStream(sid)
				if len(data) > 0 && !writeStreamData(conn, data) {
					return
				}

				if c.mux.StreamClosed(sid) {
					return
				}
			}
		}
	}()

	select {
	case <-done:
	case <-streamClosed:
	}
}

func writeStreamData(conn net.Conn, data []byte) bool {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return false
		}
		data = data[n:]
	}

	return true
}

func writeResponse(conn net.Conn, response []byte) {
	if _, err := conn.Write(response); err != nil {
		logger.Debugf("SOCKS5 response write error: %v", err)
	}
}

func replyUnsupportedSOCKSMethod() []byte {
	return []byte{5, 0xFF}
}

func replyAuthFailed() []byte {
	return []byte{0x01, 0x01}
}

func replyAuthOK() []byte {
	return []byte{0x01, 0x00}
}

func replyCommandNotSupported() []byte {
	return []byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0}
}

func replyAddressNotSupported() []byte {
	return []byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0}
}

func replyHostUnreachable() []byte {
	return []byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0}
}

func replySuccess() []byte {
	return []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
}
