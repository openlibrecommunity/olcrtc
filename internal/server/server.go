package server

import (
	"context"
	"crypto/rand"
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
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
	"github.com/pion/webrtc/v4"
)

type Server struct {
	peers          []*telemost.Peer
	cipher         *crypto.Cipher
	mux            *mux.Multiplexer
	connections    map[uint16]net.Conn
	connMu         sync.RWMutex
	streamPumps    map[uint16]net.Conn
	pumpMu         sync.Mutex
	peerIdx        atomic.Uint32
	activeClients  atomic.Int32
	wg             sync.WaitGroup
	dnsServer      string
	dnsCache       sync.Map
	resolver       *net.Resolver
	socksProxyAddr string
	socksProxyPort int
}

type ConnectRequest struct {
	Cmd  string `json:"cmd"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

func Run(ctx context.Context, roomURL, keyHex string, dnsServer, socksProxyAddr string, socksProxyPort int) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var key []byte
	var err error

	if keyHex == "" {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		log.Printf("Generated key: %x", key)
	} else {
		key, err = hex.DecodeString(keyHex)
		if err != nil {
			return err
		}
		if len(key) != 32 {
			return fmt.Errorf("key must be 32 bytes, got %d", len(key))
		}
	}

	keyStr := string(key)
	if len(keyStr) != 32 {
		return fmt.Errorf("key string length must be 32, got %d", len(keyStr))
	}

	cipher, err := crypto.NewCipher(keyStr)
	if err != nil {
		return err
	}

	s := &Server{
		cipher:         cipher,
		connections:    make(map[uint16]net.Conn),
		streamPumps:    make(map[uint16]net.Conn),
		peers:          make([]*telemost.Peer, 0),
		dnsServer:      dnsServer,
		socksProxyAddr: socksProxyAddr,
		socksProxyPort: socksProxyPort,
	}

	if dnsServer == "" {
		dnsServer = "1.1.1.1:53"
	}

	s.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, dnsServer)
		},
	}

	peerCount := 1

	s.mux = mux.New(0, func(frame []byte) error {
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
			return err
		}
		idx := s.peerIdx.Add(1) % uint32(len(s.peers))
		return s.peers[idx].Send(encrypted)
	})

	for i := 0; i < peerCount; i++ {
		peerID := i
		peer, err := telemost.NewPeer(roomURL, names.Generate(), s.onData)
		if err != nil {
			return err
		}
		peer.SetEndedCallback(func(reason string) {
			log.Printf("Server peer %d reported conference end: %s", peerID, reason)
			cancel()
		})
		s.peers = append(s.peers, peer)

		peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
			if dc == nil {
				log.Printf("Server peer %d channel closed - resetting multiplexer state", peerID)
			} else {
				log.Printf("Server peer %d reconnected - resetting multiplexer state", peerID)
			}

			s.connMu.Lock()
			for sid, conn := range s.connections {
				if conn != nil {
					conn.Close()
				}
				delete(s.connections, sid)
			}
			s.connMu.Unlock()

			if dc != nil {
				s.mux.UpdateSendFunc(func(frame []byte) error {
					encrypted, err := s.cipher.Encrypt(frame)
					if err != nil {
						return err
					}
					idx := s.peerIdx.Add(1) % uint32(len(s.peers))
					return s.peers[idx].Send(encrypted)
				})
			}

			s.mux.Reset()

			log.Println("Server multiplexer reset complete")
		})

		peer.SetShouldReconnect(func() bool {
			return s.activeClients.Load() > 0
		})

		log.Printf("Connecting peer %d to Telemost...", peerID)
		if err := peer.Connect(runCtx); err != nil {
			return err
		}
		log.Printf("Peer %d connected", peerID)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			peer.WatchConnection(runCtx)
		}()
	}

	err = s.run(runCtx)

	log.Println("Waiting for server goroutines...")
	s.wg.Wait()
	log.Println("Server goroutines finished")

	return err
}

func (s *Server) socks5Connect(conn net.Conn, targetAddr string, targetPort int) error {
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		return err
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 5 || resp[1] != 0 {
		return fmt.Errorf("SOCKS5 auth failed")
	}

	req := []byte{5, 1, 0, 3}
	req = append(req, byte(len(targetAddr)))
	req = append(req, []byte(targetAddr)...)
	req = append(req, byte(targetPort>>8), byte(targetPort))

	if _, err := conn.Write(req); err != nil {
		return err
	}

	resp = make([]byte, 10)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return err
	}
	if resp[0] != 5 || resp[1] != 0 {
		return fmt.Errorf("SOCKS5 connect failed: %d", resp[1])
	}

	return nil
}

func (s *Server) onData(data []byte) {
	plaintext, err := s.cipher.Decrypt(data)
	if err != nil {
		logger.Debug("Decrypt error: %v", err)
		return
	}

	if control, ok := mux.ParseControlFrame(plaintext); ok && control.Type == mux.ControlResetClient {
		log.Printf("Received reset signal from client (clientID=%d) - cleaning up", control.ClientID)
		s.closeClientConnections(control.ClientID)
	}

	s.mux.HandleFrame(plaintext)
}

func (s *Server) closeClientConnections(clientID uint32) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	for streamSid, conn := range s.connections {
		stream := s.mux.GetStream(streamSid)
		if stream != nil && stream.ClientID == clientID {
			if conn != nil {
				conn.Close()
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
					conn.Close()
				}
			}
			s.connMu.Unlock()

			log.Printf("Closing %d peer(s)...", len(s.peers))
			for i, peer := range s.peers {
				log.Printf("Closing peer %d...", i)
				peer.Close()
			}
			log.Println("All peers closed")

			return nil

		case <-ticker.C:
		}
		sids := s.mux.GetStreams()

		for _, sid := range sids {
			if s.mux.StreamClosed(sid) {
				s.closeStreamConnection(sid)
				continue
			}

			if s.hasConnection(sid) {
				continue
			}

			data := s.mux.ReadStream(sid)
			if len(data) == 0 {
				continue
			}

			var req ConnectRequest
			if err := json.Unmarshal(data, &req); err == nil && req.Cmd == "connect" {
				log.Printf("[SERVER] sid=%d RECEIVED_CONNECT_REQUEST %s:%d", sid, req.Addr, req.Port)
				s.closeStreamConnection(sid)
				go s.handleConnect(ctx, sid, req)
			}
		}
	}
}

func (s *Server) hasConnection(sid uint16) bool {
	s.connMu.RLock()
	defer s.connMu.RUnlock()
	conn := s.connections[sid]
	return conn != nil
}

func (s *Server) closeStreamConnection(sid uint16) {
	s.connMu.Lock()
	conn := s.connections[sid]
	if conn != nil {
		conn.Close()
		delete(s.connections, sid)
	}
	s.connMu.Unlock()
}

func (s *Server) closeStreamConnectionIfCurrent(sid uint16, expected net.Conn) {
	s.connMu.Lock()
	conn := s.connections[sid]
	if conn == expected {
		conn.Close()
		delete(s.connections, sid)
	}
	s.connMu.Unlock()
}

func (s *Server) markStreamPump(sid uint16, conn net.Conn) bool {
	s.pumpMu.Lock()
	defer s.pumpMu.Unlock()
	if current := s.streamPumps[sid]; current == conn {
		return false
	} else if current != nil {
		current.Close()
	}
	s.streamPumps[sid] = conn
	return true
}

func (s *Server) unmarkStreamPump(sid uint16, conn net.Conn) {
	s.pumpMu.Lock()
	if s.streamPumps[sid] == conn {
		delete(s.streamPumps, sid)
	}
	s.pumpMu.Unlock()
}

func (s *Server) handleConnect(ctx context.Context, sid uint16, req ConnectRequest) {
	startTime := time.Now()
	addr := fmt.Sprintf("%s:%d", req.Addr, req.Port)
	logger.Verbose("Handling connect request sid=%d to %s", sid, addr)
	log.Printf("[SERVER] sid=%d CONNECT_START %s", sid, addr)

	s.connMu.Lock()
	oldConn, exists := s.connections[sid]
	if exists && oldConn != nil {
		log.Printf("Closing old connection for sid=%d", sid)
		oldConn.Close()
		delete(s.connections, sid)
	}
	s.connMu.Unlock()

	dialStart := time.Now()
	var conn net.Conn
	var err error

	if s.socksProxyAddr == "" {
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
			Resolver:  s.resolver,
		}
		conn, err = dialer.Dial("tcp4", addr)
		logger.Verbose("TCP dial took %v for sid=%d (direct)", time.Since(dialStart), sid)
	} else {
		proxyAddr := fmt.Sprintf("%s:%d", s.socksProxyAddr, s.socksProxyPort)
		dialer := &net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}
		conn, err = dialer.Dial("tcp4", proxyAddr)
		if err == nil {
			if err := s.socks5Connect(conn, req.Addr, req.Port); err != nil {
				conn.Close()
				err = fmt.Errorf("SOCKS5 connect failed: %v", err)
			}
		}
		logger.Verbose("SOCKS5 proxy dial took %v for sid=%d", time.Since(dialStart), sid)
	}
	dialElapsed := time.Since(dialStart)

	if err != nil {
		log.Printf("[SERVER] sid=%d CONNECT_FAILED dial_time=%v total_elapsed=%v err=%v", sid, dialElapsed, time.Since(startTime), err)
		go s.mux.CloseStream(sid)
		return
	}

	logger.Verbose("TCP dial took %v for sid=%d", dialElapsed, sid)
	s.connMu.Lock()
	s.connections[sid] = conn
	s.connMu.Unlock()

	log.Printf("[SERVER] sid=%d CONNECT_SUCCESS dial_time=%v", sid, dialElapsed)

	s.activeClients.Add(1)
	s.mux.SendData(sid, []byte{0x00})
	s.startStreamPump(ctx, sid, conn)

	go func() {
		defer func() {
			s.activeClients.Add(-1)
			s.mux.CloseStream(sid)
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

			totalSent += uint64(n)
			if time.Since(lastLog) > 5*time.Second {
				log.Printf("[SERVER] sid=%d TRANSFER_PROGRESS sent=%d MB", sid, totalSent/(1024*1024))
				lastLog = time.Now()
			}
		}
	}()
}

func (s *Server) startStreamPump(ctx context.Context, sid uint16, conn net.Conn) {
	if !s.markStreamPump(sid, conn) {
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.unmarkStreamPump(sid, conn)

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				data := s.mux.ReadStream(sid)
				if len(data) > 0 {
					if _, err := conn.Write(data); err != nil {
						s.mux.CloseStream(sid)
						s.closeStreamConnectionIfCurrent(sid, conn)
						return
					}
				}
				if s.mux.StreamClosed(sid) {
					s.closeStreamConnectionIfCurrent(sid, conn)
					return
				}
			}
		}
	}()
}

func (s *Server) canSendData() bool {
	for _, peer := range s.peers {
		if !peer.CanSend() {
			return false
		}
	}
	return true
}
