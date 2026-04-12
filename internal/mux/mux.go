// Package mux provides a multiplexer for multiple streams over a single connection.
package mux

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
)

var (
	ErrClientResetID = errors.New("client reset requires a non-zero client id") //nolint:revive
)

const (
	ControlStreamID uint16 = 0xFFFF //nolint:revive
	ControlLength   uint16 = 0xFFFF //nolint:revive

	ControlResetClient uint32 = 1
)

type ControlFrame struct { //nolint:revive
	ClientID uint32
	Type     uint32
}

type Stream struct { //nolint:revive
	ID         uint16
	ClientID   uint32
	recvBuf    []byte
	closed     bool
	mu         sync.Mutex
	nextSeq    uint32
	outOfOrder map[uint32][]byte
}

func (s *Stream) RecvBuf() []byte { //nolint:revive
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recvBuf
}

type Multiplexer struct { //nolint:revive
	streams       map[uint16]*Stream
	nextID        uint16
	clientID      uint32
	onSend        func([]byte) error
	mu            sync.RWMutex
	maxStreams    int
	maxBufferSize int
	dataReady     map[uint16]chan struct{}
	dataReadyMu   sync.Mutex
	sendSeq       map[uint16]uint32
	sendSeqMu     sync.Mutex
}

func New(clientID uint32, onSend func([]byte) error) *Multiplexer { //nolint:revive
	return &Multiplexer{
		streams:       make(map[uint16]*Stream),
		nextID:        1,
		clientID:      clientID,
		onSend:        onSend,
		maxStreams:    10000,
		maxBufferSize: 32 * 1024 * 1024,
		dataReady:     make(map[uint16]chan struct{}),
		sendSeq:       make(map[uint16]uint32),
	}
}

func (m *Multiplexer) OpenStream() uint16 { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	for {
		sid := m.nextID
		m.nextID++
		if m.nextID == 0 {
			m.nextID = 1
		}

		if _, exists := m.streams[sid]; !exists {
			m.streams[sid] = &Stream{
				ID:         sid,
				recvBuf:    make([]byte, 0),
				nextSeq:    0,
				outOfOrder: make(map[uint32][]byte),
			}
			return sid
		}
	}
}

func (m *Multiplexer) SendData(sid uint16, data []byte) error { //nolint:revive
	m.mu.RLock()
	stream, exists := m.streams[sid]
	m.mu.RUnlock()

	if !exists || stream.closed {
		return nil
	}

	const chunkSize = 7000
	totalChunks := (len(data) + chunkSize - 1) / chunkSize

	if totalChunks > 10 {
		logger.Debugf("SendData: sid=%d, size=%d bytes, chunks=%d", sid, len(data), totalChunks)
	}

	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[i:end]

		m.sendSeqMu.Lock()
		seq := m.sendSeq[sid]
		m.sendSeq[sid]++
		m.sendSeqMu.Unlock()

		frame := make([]byte, 12+len(chunk))
		binary.BigEndian.PutUint32(frame[0:4], m.clientID)
		binary.BigEndian.PutUint16(frame[4:6], sid)
		binary.BigEndian.PutUint16(frame[6:8], uint16(uint32(len(chunk)))) //nolint:gosec
		binary.BigEndian.PutUint32(frame[8:12], seq)
		copy(frame[12:], chunk)

		if err := m.onSend(frame); err != nil {
			return fmt.Errorf("onSend failed: %w", err)
		}
	}

	return nil
}

func (m *Multiplexer) CloseStream(sid uint16) error { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	if stream, exists := m.streams[sid]; exists {
		stream.closed = true
	}

	m.sendSeqMu.Lock()
	delete(m.sendSeq, sid)
	m.sendSeqMu.Unlock()

	frame := make([]byte, 12)
	binary.BigEndian.PutUint32(frame[0:4], m.clientID)
	binary.BigEndian.PutUint16(frame[4:6], sid)
	binary.BigEndian.PutUint16(frame[6:8], 0)
	binary.BigEndian.PutUint32(frame[8:12], 0)

	if err := m.onSend(frame); err != nil {
		return fmt.Errorf("onSend failed: %w", err)
	}
	return nil
}

func (m *Multiplexer) SendClientReset() error { //nolint:revive
	if m.clientID == 0 {
		return ErrClientResetID
	}
	if err := m.onSend(BuildControlFrame(m.clientID, ControlResetClient)); err != nil {
		return fmt.Errorf("onSend failed: %w", err)
	}
	return nil
}

func BuildControlFrame(clientID uint32, controlType uint32) []byte { //nolint:revive
	frame := make([]byte, 12)
	binary.BigEndian.PutUint32(frame[0:4], clientID)
	binary.BigEndian.PutUint16(frame[4:6], ControlStreamID)
	binary.BigEndian.PutUint16(frame[6:8], ControlLength)
	binary.BigEndian.PutUint32(frame[8:12], controlType)
	return frame
}

func ParseControlFrame(frame []byte) (ControlFrame, bool) { //nolint:revive
	if len(frame) < 12 {
		return ControlFrame{}, false
	}

	sid := binary.BigEndian.Uint16(frame[4:6])
	length := binary.BigEndian.Uint16(frame[6:8])
	if sid != ControlStreamID || length != ControlLength {
		return ControlFrame{}, false
	}

	return ControlFrame{
		ClientID: binary.BigEndian.Uint32(frame[0:4]),
		Type:     binary.BigEndian.Uint32(frame[8:12]),
	}, true
}

func (m *Multiplexer) HandleFrame(frame []byte) { //nolint:revive
	control, ok := ParseControlFrame(frame)
	if ok {
		m.handleControlFrame(control)
		return
	}

	if len(frame) < 12 {
		return
	}

	clientID := binary.BigEndian.Uint32(frame[0:4])
	sid := binary.BigEndian.Uint16(frame[4:6])
	length := binary.BigEndian.Uint16(frame[6:8])
	seq := binary.BigEndian.Uint32(frame[8:12])

	if length == 0 {
		m.handleCloseStreamFrame(sid, clientID)
		return
	}

	if len(frame) < 12+int(length) {
		return
	}

	m.processDataFrame(sid, clientID, seq, frame[12:12+length])
}

func (m *Multiplexer) handleCloseStreamFrame(sid uint16, clientID uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if stream, exists := m.streams[sid]; exists && stream.ClientID == clientID {
		stream.closed = true
	}
}

func (m *Multiplexer) processDataFrame(sid uint16, clientID uint32, seq uint32, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stream := m.getOrCreateStream(sid, clientID)
	if stream == nil {
		return
	}

	if seq == stream.nextSeq {
		if s := m.waitForBufferSpace(sid, clientID, len(data)); s != nil {
			s.recvBuf = append(s.recvBuf, data...)
			s.nextSeq++
			m.applyOutOfOrder(s, sid, clientID)
			m.notifyDataReady(sid)
		}
	} else if seq > stream.nextSeq {
		if len(stream.outOfOrder) < 100 {
			stream.outOfOrder[seq] = append([]byte(nil), data...)
		}
	}
}

func (m *Multiplexer) getOrCreateStream(sid uint16, clientID uint32) *Stream {
	stream, exists := m.streams[sid]
	if !exists {
		if len(m.streams) >= m.maxStreams {
			return nil
		}
		stream = &Stream{
			ID:         sid,
			ClientID:   clientID,
			recvBuf:    make([]byte, 0),
			nextSeq:    0,
			outOfOrder: make(map[uint32][]byte),
		}
		m.streams[sid] = stream
		return stream
	}

	if stream.ClientID != clientID {
		stream.ClientID = clientID
		stream.recvBuf = make([]byte, 0)
		stream.closed = false
		stream.nextSeq = 0
		stream.outOfOrder = make(map[uint32][]byte)
	}
	return stream
}

func (m *Multiplexer) applyOutOfOrder(stream *Stream, sid uint16, clientID uint32) {
	for {
		nextData, ok := stream.outOfOrder[stream.nextSeq]
		if !ok {
			break
		}
		if s := m.waitForBufferSpace(sid, clientID, len(nextData)); s == nil {
			return
		}
		stream.recvBuf = append(stream.recvBuf, nextData...)
		delete(stream.outOfOrder, stream.nextSeq)
		stream.nextSeq++
		logger.Verbosef("Applied out-of-order packet sid=%d seq=%d", sid, stream.nextSeq-1)
	}
}

func (m *Multiplexer) notifyDataReady(sid uint16) {
	m.dataReadyMu.Lock()
	defer m.dataReadyMu.Unlock()
	if ch, ok := m.dataReady[sid]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (m *Multiplexer) handleControlFrame(control ControlFrame) {
	switch control.Type {
	case ControlResetClient:
		m.ResetClient(control.ClientID)
	default:
		logger.Debugf("Unknown mux control frame type=%d clientID=%d", control.Type, control.ClientID)
	}
}

func (m *Multiplexer) ResetClient(clientID uint32) { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	for streamSid, stream := range m.streams {
		if stream.ClientID == clientID {
			stream.closed = true
			delete(m.streams, streamSid)
		}
	}
}

func (m *Multiplexer) waitForBufferSpace(sid uint16, clientID uint32, need int) *Stream {
	for {
		stream, ok := m.streams[sid]
		if !ok || stream.ClientID != clientID || stream.closed {
			return nil
		}
		if len(stream.recvBuf)+need <= m.maxBufferSize {
			return stream
		}
		m.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		m.mu.Lock()
	}
}

func (m *Multiplexer) ReadStream(sid uint16) []byte { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	stream, exists := m.streams[sid]
	if !exists || len(stream.recvBuf) == 0 {
		return nil
	}

	data := stream.recvBuf
	stream.recvBuf = make([]byte, 0)
	return data
}

func (m *Multiplexer) StreamClosed(sid uint16) bool { //nolint:revive
	m.mu.RLock()
	defer m.mu.RUnlock()

	stream, exists := m.streams[sid]
	return !exists || stream.closed
}

func (m *Multiplexer) GetStreams() []uint16 { //nolint:revive
	m.mu.RLock()
	defer m.mu.RUnlock()

	sids := make([]uint16, 0, len(m.streams))
	for sid := range m.streams {
		sids = append(sids, sid)
	}
	return sids
}

func (m *Multiplexer) GetStream(sid uint16) *Stream { //nolint:revive
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[sid]
}

func (m *Multiplexer) Reset() { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, stream := range m.streams {
		stream.closed = true
	}

	m.streams = make(map[uint16]*Stream)
	m.nextID = 1

	m.sendSeqMu.Lock()
	m.sendSeq = make(map[uint16]uint32)
	m.sendSeqMu.Unlock()
}

func (m *Multiplexer) UpdateSendFunc(onSend func([]byte) error) { //nolint:revive
	m.mu.Lock()
	defer m.mu.Unlock()

	m.onSend = onSend
}

func (m *Multiplexer) WaitForData(sid uint16) <-chan struct{} { //nolint:revive
	m.dataReadyMu.Lock()
	defer m.dataReadyMu.Unlock()

	if _, ok := m.dataReady[sid]; !ok {
		m.dataReady[sid] = make(chan struct{}, 1)
	}
	return m.dataReady[sid]
}

func (m *Multiplexer) CleanupDataChannel(sid uint16) { //nolint:revive
	m.dataReadyMu.Lock()
	defer m.dataReadyMu.Unlock()

	if ch, ok := m.dataReady[sid]; ok {
		close(ch)
		delete(m.dataReady, sid)
	}
}
