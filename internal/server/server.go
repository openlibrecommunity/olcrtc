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

type ConnectRequest struct {
	Cmd  string `json:"cmd"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

func Run(ctx context.Context, roomURL, keyHex string, duo bool, dnsServer string) error {
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
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, dnsServer)
		},
	}

	peerCount := 1
	if duo {
		peerCount = 2
		log.Println("Duo mode: using 2 parallel channels")
	}

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
		peer, err := telemost.NewPeer(roomURL, names.Generate(), s.onData)
		if err != nil {
			return err
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

		log.Printf("Connecting peer %d to Telemost...", i)
		if err := peer.Connect(ctx); err != nil {
			return err
		}
		log.Printf("Peer %d connected", i)

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			peer.WatchConnection(ctx)
		}()
	}

	err = s.run(ctx)

	log.Println("Waiting for server goroutines...")
	s.wg.Wait()
	log.Println("Server goroutines finished")

	return err
}

func (s *Server) onData(data []byte) {
	plaintext, err := s.cipher.Decrypt(data)
	if err != nil {
		logger.Debug("Decrypt error: %v", err)
		return
	}

	if len(plaintext) >= 12 {
		clientID := binary.BigEndian.Uint32(plaintext[0:4])
		sid := binary.BigEndian.Uint16(plaintext[4:6])
		length := binary.BigEndian.Uint16(plaintext[6:8])

		if sid == 0xFFFF && length == 0xFFFF {
			log.Printf("Received reset signal from client (clientID=%d) - cleaning up", clientID)
			s.connMu.Lock()
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
			s.connMu.Unlock()
		}
	} else if len(plaintext) >= 8 {
		clientID := binary.BigEndian.Uint32(plaintext[0:4])
		sid := binary.BigEndian.Uint16(plaintext[4:6])
		length := binary.BigEndian.Uint16(plaintext[6:8])

		if sid == 0xFFFF && length == 0xFFFF {
			log.Printf("Received reset signal from client (clientID=%d) - cleaning up", clientID)
			s.connMu.Lock()
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
			s.connMu.Unlock()
		}
	}

	s.mux.HandleFrame(plaintext)
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

		sids := s.mux.GetStreams()

		for _, sid := range sids {
			go func(sid uint16) {
				data := s.mux.ReadStream(sid)
				if len(data) > 0 {
					s.connMu.RLock()
					conn, exists := s.connections[sid]
					s.connMu.RUnlock()

					if exists && conn != nil {
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
					} else {
						var req ConnectRequest
						if err := json.Unmarshal(data, &req); err == nil && req.Cmd == "connect" {
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
					}
				}

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
			}(sid)
		}
	}
}

func (s *Server) handleConnect(sid uint16, req ConnectRequest) {
	startTime := time.Now()
	addr := net.JoinHostPort(req.Addr, fmt.Sprintf("%d", req.Port))
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
		log.Printf("[SERVER] sid=%d CONNECT_FAILED dial_time=%v total_elapsed=%v err=%v", sid, dialElapsed, time.Since(startTime), err)
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

	go func() {
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

			totalSent += uint64(n)
			if time.Since(lastLog) > 5*time.Second {
				log.Printf("[SERVER] sid=%d TRANSFER_PROGRESS sent=%d MB", sid, totalSent/(1024*1024))
				lastLog = time.Now()
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
