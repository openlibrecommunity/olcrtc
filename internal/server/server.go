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
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
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

func Run(ctx context.Context, roomURL, keyHex string) error {
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
	}

	s.mux = mux.New(0, func(frame []byte) error {
		encrypted, err := s.cipher.Encrypt(frame)
		if err != nil {
			return err
		}
		return s.peer.Send(encrypted)
	})

	peer, err := telemost.NewPeer(roomURL, names.Generate(), s.onData)
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
		
		if dc != nil {
			s.mux.UpdateSendFunc(func(frame []byte) error {
				encrypted, err := s.cipher.Encrypt(frame)
				if err != nil {
					return err
				}
				return s.peer.Send(encrypted)
			})
		}
		
		s.mux.Reset()
		
		log.Println("Server multiplexer reset complete")
	})

	log.Println("Connecting to Telemost...")
	if err := peer.Connect(ctx); err != nil {
		return err
	}
	log.Println("Connected to Telemost")

	go peer.WatchConnection(ctx)

	return s.run(ctx)
}

func (s *Server) onData(data []byte) {
	plaintext, err := s.cipher.Decrypt(data)
	if err != nil {
		logger.Debug("Decrypt error: %v", err)
		return
	}

	logger.Verbose("Received %d bytes from client", len(plaintext))

	if len(plaintext) >= 8 {
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
						conn.Close()
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
			s.peer.Close()
			return nil
		default:
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
							s.mux.CloseStream(sid)
							conn.Close()
							s.connMu.Lock()
							delete(s.connections, sid)
							s.connMu.Unlock()
						}
					} else {
						var req ConnectRequest
						if err := json.Unmarshal(data, &req); err == nil && req.Cmd == "connect" {
							s.connMu.Lock()
							if oldConn, exists := s.connections[sid]; exists && oldConn != nil {
								oldConn.Close()
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
						conn.Close()
						delete(s.connections, sid)
					}
					s.connMu.Unlock()
				}
			}(sid)
		}
		
		time.Sleep(1 * time.Millisecond)
	}
}

func (s *Server) handleConnect(sid uint16, req ConnectRequest) {
	addr := fmt.Sprintf("%s:%d", req.Addr, req.Port)
	logger.Verbose("Handling connect request sid=%d to %s", sid, addr)
	log.Printf("Connecting sid=%d to %s", sid, addr)

	s.connMu.Lock()
	oldConn, exists := s.connections[sid]
	if exists && oldConn != nil {
		log.Printf("Closing old connection for sid=%d", sid)
		oldConn.Close()
		delete(s.connections, sid)
	}
	s.connMu.Unlock()

	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		log.Printf("Connect failed sid=%d: %v", sid, err)
		go s.mux.CloseStream(sid)
		return
	}

	s.connMu.Lock()
	s.connections[sid] = conn
	s.connMu.Unlock()
	log.Printf("Connected sid=%d", sid)

	s.mux.SendData(sid, []byte{0x00})

	go func() {
		defer func() {
			s.mux.CloseStream(sid)
			s.connMu.Lock()
			delete(s.connections, sid)
			s.connMu.Unlock()
		}()
		
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}

			if err := s.mux.SendData(sid, buf[:n]); err != nil {
				return
			}
		}
	}()
}
