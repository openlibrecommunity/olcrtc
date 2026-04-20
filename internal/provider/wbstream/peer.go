// Package wbstream implements the WB Stream WebRTC provider.
package wbstream

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
)

const (
	wsURL = "wss://wbstream01-el.wb.ru:7880"
)

// Peer represents a WB Stream WebRTC connection using LiveKit.
type Peer struct {
	roomURL         string
	name            string
	room            *lksdk.Room
	onData          func([]byte)
	onReconnect     func(*webrtc.DataChannel)
	shouldReconnect func() bool
	onEnded         func(string)
	sendQueue       chan []byte
	closed          atomic.Bool
	ctx             context.Context
	cancel          context.CancelFunc
	wg              sync.WaitGroup
}

// NewPeer creates a new WB Stream provider peer.
func NewPeer(ctx context.Context, roomURL, name string, onData func([]byte)) (*Peer, error) {
	childCtx, cancel := context.WithCancel(ctx)
	return &Peer{
		roomURL:   roomURL,
		name:      name,
		onData:    onData,
		sendQueue: make(chan []byte, 5000),
		ctx:       childCtx,
		cancel:    cancel,
	}, nil
}

// Connect starts the WebRTC connection process.
func (p *Peer) Connect(ctx context.Context) error {
	token, err := p.getRoomToken(ctx)
	if err != nil {
		return fmt.Errorf("get room token: %w", err)
	}

	roomCB := &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnDataReceived: func(data []byte, params lksdk.DataReceiveParams) {
				if p.onData != nil {
					p.onData(data)
				}
			},
		},
		OnDisconnected: func() {
			if p.onEnded != nil {
				p.onEnded("disconnected from livekit")
			}
		},
	}

	room, err := lksdk.ConnectToRoomWithToken(wsURL, token, roomCB, lksdk.WithAutoSubscribe(true))
	if err != nil {
		return fmt.Errorf("connect to room: %w", err)
	}

	p.room = room
	p.wg.Add(1)
	go p.processSendQueue()

	return nil
}

func (p *Peer) getRoomToken(ctx context.Context) (string, error) {
	accessToken, err := RegisterGuest(ctx, p.name)
	if err != nil {
		return "", fmt.Errorf("register guest: %w", err)
	}

	roomID := p.roomURL
	if roomID == "" || roomID == "any" {
		roomID, err = CreateRoom(ctx, accessToken)
		if err != nil {
			return "", fmt.Errorf("create room: %w", err)
		}
		log.Printf("WB Stream room created: %s", roomID)
		log.Printf("To connect client use: -id %s", roomID)
	}

	if err := JoinRoom(ctx, accessToken, roomID); err != nil {
		return "", fmt.Errorf("join room: %w", err)
	}

	token, err := GetToken(ctx, accessToken, roomID, p.name)
	if err != nil {
		return "", fmt.Errorf("get token: %w", err)
	}

	return token, nil
}

func (p *Peer) processSendQueue() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case data, ok := <-p.sendQueue:
			if !ok {
				return
			}
			if err := p.room.LocalParticipant.PublishDataPacket(lksdk.UserData(data), lksdk.WithDataPublishTopic("olcrtc"), lksdk.WithDataPublishReliable(true)); err != nil {
				log.Printf("WB Stream publish data error: %v", err)
			}
		}
	}
}

// Send transmits data to the room.
func (p *Peer) Send(data []byte) error {
	if p.closed.Load() {
		return fmt.Errorf("peer closed")
	}
	select {
	case p.sendQueue <- data:
		return nil
	default:
		return fmt.Errorf("send queue full")
	}
}

// Close terminates the provider connection.
func (p *Peer) Close() error {
	if p.closed.CompareAndSwap(false, true) {
		p.cancel()
		if p.room != nil {
			p.room.Disconnect()
		}
		close(p.sendQueue)
		p.wg.Wait()
	}
	return nil
}

// SetReconnectCallback is a stub for WB Stream.
func (p *Peer) SetReconnectCallback(cb func(*webrtc.DataChannel)) {
	p.onReconnect = cb
}

// SetShouldReconnect is a stub for WB Stream.
func (p *Peer) SetShouldReconnect(fn func() bool) {
	p.shouldReconnect = fn
}

// SetEndedCallback sets the function to call when the session ends.
func (p *Peer) SetEndedCallback(cb func(string)) {
	p.onEnded = cb
}

// WatchConnection is a stub for WB Stream.
func (p *Peer) WatchConnection(ctx context.Context) {}

// CanSend checks if the provider is ready to transmit data.
func (p *Peer) CanSend() bool {
	return !p.closed.Load() && p.room != nil
}

// GetSendQueue returns the data transmission queue.
func (p *Peer) GetSendQueue() chan []byte {
	return p.sendQueue
}

// GetBufferedAmount is a stub for WB Stream.
func (p *Peer) GetBufferedAmount() uint64 {
	return 0
}
