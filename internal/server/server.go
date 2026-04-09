package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
)

type Server struct {
	peer        *telemost.Peer
	cipher      *crypto.Cipher
	mux         *mux.Multiplexer
	connections map[uint16]net.Conn
	connMu      sync.RWMutex
}

type ConnectRequest struct {
	Cmd  string `json:"cmd"`
	Addr string `json:"addr"`
	Port int    `json:"port"`
}

func Run(roomURL, keyHex string) error {
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
	}

	cipher, err := crypto.NewCipher(string(key))
	if err != nil {
		return err
	}

	s := &Server{
		cipher:      cipher,
		connections: make(map[uint16]net.Conn),
	}

	s.mux = mux.New(func(frame []byte) error {
		encrypted, err := s.cipher.Encrypt(frame)
		if err != nil {
			return err
		}
		return s.peer.Send(encrypted)
	})

	peer, err := telemost.NewPeer(roomURL, "OlcRTC-Server", s.onData)
	if err != nil {
		return err
	}
	s.peer = peer

	peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
		log.Println("Server reconnected - resetting multiplexer state")
		
		s.connMu.Lock()
		for sid, conn := range s.connections {
			if conn != nil {
				conn.Close()
			}
			delete(s.connections, sid)
		}
		s.connMu.Unlock()
		
		s.mux.UpdateSendFunc(func(frame []byte) error {
			encrypted, err := s.cipher.Encrypt(frame)
			if err != nil {
				return err
			}
			return dc.Send(encrypted)
		})
		
		s.mux.Reset()
		
		log.Println("Server multiplexer reset complete")
	})

	log.Println("Connecting to Telemost...")
	ctx := context.Background()
	if err := peer.Connect(ctx); err != nil {
		return err
	}
	log.Println("Connected to Telemost")

	go peer.WatchConnection(ctx)

	return s.run()
}

func (s *Server) onData(data []byte) {
	plaintext, err := s.cipher.Decrypt(data)
	if err != nil {
		return
	}

	s.mux.HandleFrame(plaintext)
}

func (s *Server) run() error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		for _, sid := range s.mux.GetStreams() {
			data := s.mux.ReadStream(sid)
			if len(data) > 0 {
				s.connMu.RLock()
				conn, exists := s.connections[sid]
				s.connMu.RUnlock()
				
				if exists {
					if _, err := conn.Write(data); err != nil {
						s.mux.CloseStream(sid)
						conn.Close()
						s.connMu.Lock()
						delete(s.connections, sid)
						s.connMu.Unlock()
					}
				} else {
					var req ConnectRequest
					if err := json.Unmarshal(data, &req); err == nil && req.Cmd == "connect" {
						go s.handleConnect(sid, req)
					}
				}
			}

			if s.mux.StreamClosed(sid) {
				s.connMu.RLock()
				conn, exists := s.connections[sid]
				s.connMu.RUnlock()
				
				if exists {
					conn.Close()
					s.connMu.Lock()
					delete(s.connections, sid)
					s.connMu.Unlock()
				}
			}
		}
	}

	return nil
}

func (s *Server) handleConnect(sid uint16, req ConnectRequest) {
	addr := fmt.Sprintf("%s:%d", req.Addr, req.Port)
	log.Printf("Connecting sid=%d to %s", sid, addr)

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("Connect failed sid=%d: %v", sid, err)
		s.mux.CloseStream(sid)
		return
	}

	s.connMu.Lock()
	s.connections[sid] = conn
	s.connMu.Unlock()
	log.Printf("Connected sid=%d", sid)

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				s.mux.CloseStream(sid)
				return
			}

			if err := s.mux.SendData(sid, buf[:n]); err != nil {
				return
			}
		}
	}()
}
