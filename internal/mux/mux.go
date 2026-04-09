// ===========================================
// AI GENERATED / AI GENERATED / AI GENERATED
//===========================================

package mux

import (
	"encoding/binary"
	"sync"
)

type Stream struct {
	ID       uint16
	ClientID uint32
	recvBuf  []byte
	closed   bool
	mu       sync.Mutex
}

type Multiplexer struct {
	streams    map[uint16]*Stream
	nextID     uint16
	clientID   uint32
	onSend     func([]byte) error
	mu         sync.RWMutex
	maxStreams int
}

func New(clientID uint32, onSend func([]byte) error) *Multiplexer {
	return &Multiplexer{
		streams:    make(map[uint16]*Stream),
		nextID:     1,
		clientID:   clientID,
		onSend:     onSend,
		maxStreams: 10000,
	}
}

func (m *Multiplexer) OpenStream() uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()

	sid := m.nextID
	m.nextID++

	m.streams[sid] = &Stream{
		ID:     sid,
		recvBuf: make([]byte, 0),
	}

	return sid
}

func (m *Multiplexer) SendData(sid uint16, data []byte) error {
	m.mu.RLock()
	stream, exists := m.streams[sid]
	m.mu.RUnlock()

	if !exists || stream.closed {
		return nil
	}

	const chunkSize = 7168
	for i := 0; i < len(data); i += chunkSize {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}

		chunk := data[i:end]
		frame := make([]byte, 8+len(chunk))
		binary.BigEndian.PutUint32(frame[0:4], m.clientID)
		binary.BigEndian.PutUint16(frame[4:6], sid)
		binary.BigEndian.PutUint16(frame[6:8], uint16(len(chunk)))
		copy(frame[8:], chunk)

		if err := m.onSend(frame); err != nil {
			return err
		}
	}

	return nil
}

func (m *Multiplexer) CloseStream(sid uint16) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if stream, exists := m.streams[sid]; exists {
		stream.closed = true
	}

	frame := make([]byte, 8)
	binary.BigEndian.PutUint32(frame[0:4], m.clientID)
	binary.BigEndian.PutUint16(frame[4:6], sid)
	binary.BigEndian.PutUint16(frame[6:8], 0)

	return m.onSend(frame)
}

func (m *Multiplexer) HandleFrame(frame []byte) {
	if len(frame) < 8 {
		return
	}

	clientID := binary.BigEndian.Uint32(frame[0:4])
	sid := binary.BigEndian.Uint16(frame[4:6])
	length := binary.BigEndian.Uint16(frame[6:8])

	if sid == 0xFFFF && length == 0xFFFF {
		m.mu.Lock()
		for streamSid, stream := range m.streams {
			if stream.ClientID == clientID {
				stream.closed = true
				delete(m.streams, streamSid)
			}
		}
		m.mu.Unlock()
		return
	}

	if length == 0 {
		m.mu.Lock()
		if stream, exists := m.streams[sid]; exists && stream.ClientID == clientID {
			stream.closed = true
		}
		m.mu.Unlock()
		return
	}

	if len(frame) < 8+int(length) {
		return
	}

	data := frame[8 : 8+length]

	m.mu.Lock()
	stream, exists := m.streams[sid]
	if !exists {
		if len(m.streams) >= m.maxStreams {
			m.mu.Unlock()
			return
		}
		stream = &Stream{
			ID:       sid,
			ClientID: clientID,
			recvBuf:  make([]byte, 0),
		}
		m.streams[sid] = stream
	} else if stream.ClientID != clientID {
		stream.ClientID = clientID
		stream.recvBuf = make([]byte, 0)
		stream.closed = false
	}
	stream.recvBuf = append(stream.recvBuf, data...)
	m.mu.Unlock()
}

func (m *Multiplexer) ReadStream(sid uint16) []byte {
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

func (m *Multiplexer) StreamClosed(sid uint16) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stream, exists := m.streams[sid]
	return !exists || stream.closed
}

func (m *Multiplexer) GetStreams() []uint16 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sids := make([]uint16, 0, len(m.streams))
	for sid := range m.streams {
		sids = append(sids, sid)
	}
	return sids
}

func (m *Multiplexer) GetStream(sid uint16) *Stream {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streams[sid]
}

func (m *Multiplexer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, stream := range m.streams {
		stream.closed = true
	}
	
	m.streams = make(map[uint16]*Stream)
	m.nextID = 1
}

func (m *Multiplexer) UpdateSendFunc(onSend func([]byte) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	
	m.onSend = onSend
}
