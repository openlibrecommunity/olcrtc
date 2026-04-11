//nolint:revive
package server

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"strconv"
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
type Server struct {
	peers       []*telemost.Peer
	cipher      *crypto.Cipher
	mux         *mux.Multiplexer
	connections map[uint16]net.Conn
	connMu      sync.RWMutex
	peerIdx     atomic.Uint32
	wg          sync.WaitGroup
	dnsServer   string
	resolver    *net.Resolver
}

//nolint:revive
type ConnectRequest struct {
	Cmd  string `json:"cmd"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

//nolint:revive
func Run(ctx context.Context, roomURL, keyHex string, duo bool, dnsServer string) error {
	_, cipher, err := initCipher(keyHex)
	if err != nil {
		return err
	}

	s := &Server{
		cipher:      cipher,
		connections: make(map[uint16]net.Conn),
		peers:       make([]*telemost.Peer, 0),
		dnsServer:   dnsServer,
	}

	if dnsServer == "" {
		dnsServer = "1.1.1.1:53"
	}

	s.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, dnsServer)
		},
	}

	peerCount := 1
	if duo {
		peerCount = 2
		log.Println("Duo mode: using 2 parallel channels")
	}

	s.mux = mux.New(0, s.createSendFunc())

	if err := s.connectPeers(ctx, roomURL, peerCount); err != nil {
		return err
	}

	err = s.run(ctx)

	log.Println("Waiting for server goroutines...")
	s.wg.Wait()
	log.Println("Server goroutines finished")

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

func (s *Server) createSendFunc() func([]byte) error {
	return func(frame []byte) error {
		for {
			canSend := true
			for _, peer := range s.peers {
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

		encrypted, err := s.cipher.Encrypt(frame)
		if err != nil {
			return fmt.Errorf("failed to encrypt frame: %w", err)
		}
		if len(s.peers) == 0 {
			return errors.ErrNoPeersAvailable
		}
		if len(s.peers) > int(^uint32(0)) {
			return errors.ErrTooManyPeers
		}
		//nolint:gosec
		peerCount := uint32(len(s.peers))
		idx := s.peerIdx.Add(1) % peerCount
		return s.peers[idx].Send(encrypted)
	}
}

func (s *Server) connectPeers(ctx context.Context, roomURL string, peerCount int) error {
	for i := range peerCount {
		peer, err := telemost.NewPeer(ctx, roomURL, names.Generate(), s.onData)
		if err != nil {
			return fmt.Errorf("failed to create peer: %w", err)
		}
		s.peers = append(s.peers, peer)

		peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
			log.Printf("Server peer %d reconnected - resetting multiplexer state", i)

			s.connMu.Lock()
			for sid, conn := range s.connections {
				if conn != nil {
					if err := conn.Close(); err != nil {
						log.Printf("Error closing connection: %v", err)
					}
				}
				delete(s.connections, sid)
			}
			s.connMu.Unlock()

			if dc != nil {
				s.mux.UpdateSendFunc(s.createSendFunc())
			}

			s.mux.Reset()

			log.Println("Server multiplexer reset complete")
		})

		log.Printf("Connecting peer %d to Telemost...", i)
		if err := peer.Connect(ctx); err != nil {
			return fmt.Errorf("failed to connect peer: %w", err)
		}
		log.Printf("Peer %d connected", i)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			peer.WatchConnection(ctx)
		}()
	}
	return nil
}

func (s *Server) onData(data []byte) {
	plaintext, err := s.cipher.Decrypt(data)
	if err != nil {
		logger.Debug("Decrypt error: %v", err)
		return
	}

	s.handlePlaintext(plaintext)
	s.mux.HandleFrame(plaintext)
}

func (s *Server) handlePlaintext(plaintext []byte) {
	if len(plaintext) >= 12 {
		s.checkResetSignal(plaintext, 12)
	} else if len(plaintext) >= 8 {
		s.checkResetSignal(plaintext, 8)
	}
}

func (s *Server) checkResetSignal(plaintext []byte, _ int) {
	clientID := binary.BigEndian.Uint32(plaintext[0:4])
	sid := binary.BigEndian.Uint16(plaintext[4:6])
	length := binary.BigEndian.Uint16(plaintext[6:8])

	if sid == 0xFFFF && length == 0xFFFF {
		s.handleResetSignal(clientID)
	}
}

func (s *Server) handleResetSignal(clientID uint32) {
	log.Printf("Received reset signal from client (clientID=%d) - cleaning up", clientID)
	s.connMu.Lock()
	defer s.connMu.Unlock()

	for streamSid, conn := range s.connections {
		stream := s.mux.GetStream(streamSid)
		if stream != nil && stream.ClientID == clientID {
			if conn != nil {
				if err := conn.Close(); err != nil {
					log.Printf("Error closing connection: %v", err)
				}
			}
			delete(s.connections, streamSid)
		}
	}
}

func (s *Server) run(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Server shutting down...")
			s.connMu.Lock()
			for _, conn := range s.connections {
				if conn != nil {
					if err := conn.Close(); err != nil {
						log.Printf("Error closing connection: %v", err)
					}
				}
			}
			s.connMu.Unlock()

			log.Printf("Closing %d peer(s)...", len(s.peers))
			for i, peer := range s.peers {
				log.Printf("Closing peer %d...", i)
				if err := peer.Close(); err != nil {
					log.Printf("Error closing peer: %v", err)
				}
			}
			log.Println("All peers closed")

			return nil

		case <-ticker.C:
		}

		s.processStreams()
	}
}

func (s *Server) processStreams() {
	sids := s.mux.GetStreams()

	for _, sid := range sids {
		go s.processStream(sid)
	}
}

func (s *Server) processStream(sid uint16) {
	data := s.mux.ReadStream(sid)
	if len(data) == 0 {
		s.checkStreamClosed(sid)
		return
	}

	s.connMu.RLock()
	conn, exists := s.connections[sid]
	s.connMu.RUnlock()

	if exists && conn != nil {
		s.writeToConnection(sid, conn, data)
	} else {
		s.handleNewConnection(sid, data)
	}

	s.checkStreamClosed(sid)
}

func (s *Server) writeToConnection(sid uint16, conn net.Conn, data []byte) {
	if _, err := conn.Write(data); err != nil {
		if err := s.mux.CloseStream(sid); err != nil {
			log.Printf("Error closing stream: %v", err)
		}
		if err := conn.Close(); err != nil {
			log.Printf("Error closing connection: %v", err)
		}
		s.connMu.Lock()
		delete(s.connections, sid)
		s.connMu.Unlock()
	}
}

func (s *Server) handleNewConnection(sid uint16, data []byte) {
	var req ConnectRequest
	if err := json.Unmarshal(data, &req); err != nil || req.Cmd != "connect" {
		return
	}

	log.Printf("[SERVER] sid=%d RECEIVED_CONNECT_REQUEST %s:%d", sid, req.Addr, req.Port)
	s.connMu.Lock()
	if oldConn, exists := s.connections[sid]; exists && oldConn != nil {
		if err := oldConn.Close(); err != nil {
			log.Printf("Error closing old connection: %v", err)
		}
	}
	s.connMu.Unlock()
	go s.handleConnect(sid, req)
}

func (s *Server) checkStreamClosed(sid uint16) {
	if s.mux.StreamClosed(sid) {
		s.connMu.Lock()
		conn, exists := s.connections[sid]
		if exists && conn != nil {
			if err := conn.Close(); err != nil {
				log.Printf("Error closing connection: %v", err)
			}
			delete(s.connections, sid)
		}
		s.connMu.Unlock()
	}
}

func (s *Server) handleConnect(sid uint16, req ConnectRequest) {
	startTime := time.Now()
	addr := net.JoinHostPort(req.Addr, strconv.Itoa(req.Port))
	logger.Verbose("Handling connect request sid=%d to %s", sid, addr)
	log.Printf("[SERVER] sid=%d CONNECT_START %s", sid, addr)

	s.connMu.Lock()
	oldConn, exists := s.connections[sid]
	if exists && oldConn != nil {
		log.Printf("Closing old connection for sid=%d", sid)
		if err := oldConn.Close(); err != nil {
			log.Printf("Error closing old connection: %v", err)
		}
		delete(s.connections, sid)
	}
	s.connMu.Unlock()

	dialStart := time.Now()

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  s.resolver,
	}

	conn, err := dialer.Dial("tcp4", addr)
	dialElapsed := time.Since(dialStart)

	if err != nil {
		elapsed := time.Since(startTime)
		log.Printf("[SERVER] sid=%d CONNECT_FAILED dial_time=%v total_elapsed=%v err=%v",
			sid, dialElapsed, elapsed, err)
		go func() {
			if err := s.mux.CloseStream(sid); err != nil {
				log.Printf("Error closing stream: %v", err)
			}
		}()
		return
	}

	logger.Verbose("TCP dial took %v for sid=%d", dialElapsed, sid)

	s.connMu.Lock()
	s.connections[sid] = conn
	s.connMu.Unlock()

	log.Printf("[SERVER] sid=%d CONNECT_SUCCESS dial_time=%v", sid, dialElapsed)

	if err := s.mux.SendData(sid, []byte{0x00}); err != nil {
		log.Printf("Error sending success response: %v", err)
		return
	}

	go s.handleConnectionRead(sid, conn)
}

func (s *Server) handleConnectionRead(sid uint16, conn net.Conn) {
	defer func() {
		if err := s.mux.CloseStream(sid); err != nil {
			log.Printf("Error closing stream: %v", err)
		}
		s.connMu.Lock()
		delete(s.connections, sid)
		s.connMu.Unlock()
	}()

	buf := make([]byte, 16384)
	totalSent := uint64(0)
	lastLog := time.Now()

	for {
		n, err := conn.Read(buf)
		if err != nil {
			if totalSent > 1024*1024 {
				log.Printf("[SERVER] sid=%d TRANSFER_COMPLETE total=%d MB", sid, totalSent/(1024*1024))
			}
			return
		}

		for !s.canSendData() {
			time.Sleep(20 * time.Millisecond)
		}

		if err := s.mux.SendData(sid, buf[:n]); err != nil {
			return
		}

		if n > 0 {
			totalSent += uint64(n)
		}
		if time.Since(lastLog) > 5*time.Second {
			log.Printf("[SERVER] sid=%d TRANSFER_PROGRESS sent=%d MB", sid, totalSent/(1024*1024))
			lastLog = time.Now()
		}
	}
}

func (s *Server) canSendData() bool {
	for _, peer := range s.peers {
		if !peer.CanSend() {
			return false
		}
	}
	return true
}
