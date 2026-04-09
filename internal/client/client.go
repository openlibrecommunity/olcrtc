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
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/openlibrecommunity/olcrtc/internal/crypto"
	"github.com/openlibrecommunity/olcrtc/internal/mux"
	"github.com/openlibrecommunity/olcrtc/internal/names"
	"github.com/openlibrecommunity/olcrtc/internal/telemost"
)

type Client struct {
	peer     *telemost.Peer
	cipher   *crypto.Cipher
	mux      *mux.Multiplexer
	clientID uint32
}

func Run(roomURL, keyHex string, socksPort int) error {
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
	}

	c.mux = mux.New(c.clientID, func(frame []byte) error {
		encrypted, err := c.cipher.Encrypt(frame)
		if err != nil {
			return err
		}
		return c.peer.Send(encrypted)
	})

	peer, err := telemost.NewPeer(roomURL, names.Generate(), c.onData)
	if err != nil {
		return err
	}
	c.peer = peer

	peer.SetReconnectCallback(func(dc *webrtc.DataChannel) {
		log.Println("Client reconnected - resetting multiplexer state")
		
		c.mux.UpdateSendFunc(func(frame []byte) error {
			encrypted, err := c.cipher.Encrypt(frame)
			if err != nil {
				return err
			}
			return c.peer.Send(encrypted)
		})
		
		c.mux.Reset()
		
		log.Println("Client multiplexer reset complete")
	})

	log.Println("Connecting to Telemost...")
	ctx := context.Background()
	if err := peer.Connect(ctx); err != nil {
		return err
	}
	log.Println("Connected to Telemost")

	time.Sleep(100 * time.Millisecond)
	
	resetFrame := make([]byte, 8)
	binary.BigEndian.PutUint32(resetFrame[0:4], c.clientID)
	binary.BigEndian.PutUint16(resetFrame[4:6], 0xFFFF)
	binary.BigEndian.PutUint16(resetFrame[6:8], 0xFFFF)
	encrypted, _ := cipher.Encrypt(resetFrame)
	peer.Send(encrypted)
	log.Printf("Sent reset signal to server (clientID=%d)", c.clientID)

	go peer.WatchConnection(ctx)

	return c.runSOCKS5(socksPort)
}

func (c *Client) onData(data []byte) {
	plaintext, err := c.cipher.Decrypt(data)
	if err != nil {
		return
	}

	c.mux.HandleFrame(plaintext)
}

func (c *Client) runSOCKS5(port int) error {
	listener, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
	}
	defer listener.Close()

	log.Printf("SOCKS5 proxy listening on 0.0.0.0:%d", port)

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("Accept error: %v", err)
			continue
		}

		go c.handleSOCKS5(conn)
	}
}

func (c *Client) handleSOCKS5(conn net.Conn) {
	defer conn.Close()

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

	conn.Write([]byte{5, 0})

	if _, err := io.ReadFull(conn, buf[:4]); err != nil {
		return
	}

	if buf[1] != 1 {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
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
		conn.Write([]byte{5, 8, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		return
	}
	port := binary.BigEndian.Uint16(buf[:2])

	sid := c.mux.OpenStream()
	log.Printf("SOCKS5 connect sid=%d %s:%d", sid, addr, port)

	req := map[string]interface{}{
		"cmd":  "connect",
		"addr": addr,
		"port": port,
	}

	reqData, _ := json.Marshal(req)
	c.mux.SendData(sid, reqData)

	connected := make(chan bool, 1)
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()

	go func() {
		for i := 0; i < 100; i++ {
			time.Sleep(50 * time.Millisecond)
			data := c.mux.ReadStream(sid)
			if len(data) > 0 || c.mux.StreamClosed(sid) {
				connected <- len(data) > 0
				return
			}
		}
		connected <- false
	}()

	select {
	case success := <-connected:
		if !success {
			conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
			return
		}
	case <-timeout.C:
		conn.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}

	conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})

	done := make(chan struct{})

	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				c.mux.CloseStream(sid)
				return
			}
			c.mux.SendData(sid, buf[:n])
		}
	}()

	go func() {
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				data := c.mux.ReadStream(sid)
				if len(data) > 0 {
					conn.Write(data)
				}

				if c.mux.StreamClosed(sid) {
					return
				}
			}
		}
	}()

	<-done
}
