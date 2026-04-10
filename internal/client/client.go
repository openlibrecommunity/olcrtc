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

	"github.com/pion/webrtc/v4"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
)

type Client struct {
	peers    []*telemost.Peer
	cipher   *crypto.Cipher
	mux      *mux.Multiplexer
	clientID uint32
	peerIdx  atomic.Uint32
	wg       sync.WaitGroup
}

func Run(ctx context.Context, roomURL, keyHex string, socksPort int, duo bool) error {
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

	c.mux = mux.New(c.clientID, func(frame []byte) error {
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
			return err
		}
		
		idx := c.peerIdx.Add(1) % uint32(len(c.peers))
		return c.peers[idx].Send(encrypted)
	})

	for i := 0; i < peerCount; i++ {
		peer, err := telemost.NewPeer(roomURL, names.Generate(), c.onData)
		if err != nil {
			return err
		}
		c.peers = append(c.peers, peer)

		peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
			log.Printf("Client peer %d reconnected - resetting multiplexer state", i)
			
			c.mux.UpdateSendFunc(func(frame []byte) error {
				encrypted, err := c.cipher.Encrypt(frame)
				if err != nil {
					return err
				}
				idx := c.peerIdx.Add(1) % uint32(len(c.peers))
				return c.peers[idx].Send(encrypted)
			})
			
			c.mux.Reset()
			
			log.Println("Client multiplexer reset complete")
		})

		log.Printf("Connecting peer %d to Telemost...", i)
		if err := peer.Connect(ctx); err != nil {
			return err
		}
		log.Printf("Peer %d connected", i)

		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			peer.WatchConnection(ctx)
		}()
	}

	time.Sleep(100 * time.Millisecond)
	
	resetFrame := make([]byte, 12)
	binary.BigEndian.PutUint32(resetFrame[0:4], c.clientID)
	binary.BigEndian.PutUint16(resetFrame[4:6], 0xFFFF)
	binary.BigEndian.PutUint16(resetFrame[6:8], 0xFFFF)
	binary.BigEndian.PutUint32(resetFrame[8:12], 0)
	encrypted, _ := cipher.Encrypt(resetFrame)
	
	for _, peer := range c.peers {
		if err := peer.Send(encrypted); err != nil {
			log.Printf("Failed to send reset signal: %v", err)
		}
	}
	log.Printf("Sent reset signal to server (clientID=%d)", c.clientID)

	err = c.runSOCKS5(ctx, socksPort)
	
	log.Println("Waiting for client goroutines...")
	c.wg.Wait()
	log.Println("Client goroutines finished")
	
	return err
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
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
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

	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}

	if buf[1] != 1 {
		if _, err := conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			log.Printf("Error writing SOCKS5 error response: %v", err)
		}
		return
	}

	var addr string
	atyp := buf[3]

	switch atyp {
	case 1:
		if _, err := io.ReadFull(conn, buf[:4]); err != nil {
			return
		}
		addr = fmt.Sprintf("%d.%d.%d.%d", buf[0], buf[1], buf[2], buf[3])
	case 3:
		if _, err := io.ReadFull(conn, buf[:1]); err != nil {
			return
		}
		length := buf[0]
		if _, err := io.ReadFull(conn, buf[:length]); err != nil {
			return
		}
		addr = string(buf[:length])
	default:
		if _, err := conn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			log.Printf("Error writing SOCKS5 address type error: %v", err)
		}
		return
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])

	sid := c.mux.OpenStream()
	logger.Verbose("SOCKS5 opened stream sid=%d for %s:%d", sid, addr, port)
	log.Printf("[CLIENT] sid=%d SOCKS5_START %s:%d", sid, addr, port)

	req := map[string]interface{}{
		"cmd":  "connect",
		"addr": addr,
		"port": port,
	}

	reqData, _ := json.Marshal(req)
	c.mux.SendData(sid, reqData)

	dataReady := c.mux.WaitForData(sid)
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	select {
	case <-dataReady:
		stream := c.mux.GetStream(sid)
		if stream == nil || len(stream.RecvBuf()) == 0 {
			conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
	case <-timeout.C:
		conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	c.mux.ReadStream(sid)

	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{})
	streamClosed := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 32768)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				c.mux.CloseStream(sid)
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
				if len(data) > 0 {
					for len(data) > 0 {
						n, err := conn.Write(data)
						if err != nil {
							return
						}
						data = data[n:]
					}
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
