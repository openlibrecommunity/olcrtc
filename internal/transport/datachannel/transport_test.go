package datachannel

import (
	"context"
	"errors"
	"testing"

	"github.com/openlibrecommunity/olcrtc/internal/engine"
	enginebuiltin "github.com/openlibrecommunity/olcrtc/internal/engine/builtin"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
	"github.com/pion/webrtc/v4"
)

var (
	errDCBoom        = errors.New("boom")
	errDCConnectBoom = errors.New("connect boom")
	errDCSendBoom    = errors.New("send boom")
	errDCCloseBoom   = errors.New("close boom")
)

type stubSession struct {
	caps        engine.Capabilities
	connectErr  error
	sendErr     error
	closeErr    error
	canSend     bool
	connectCalled bool
	sent        []byte
	watched     bool
	reconnectCB func(*webrtc.DataChannel)
	shouldFn    func() bool
	endedCB     func(string)
}

func (s *stubSession) Capabilities() engine.Capabilities { return s.caps }
func (s *stubSession) Connect(context.Context) error    { s.connectCalled = true; return s.connectErr }
func (s *stubSession) Send(data []byte) error {
	s.sent = append([]byte(nil), data...)
	return s.sendErr
}
func (s *stubSession) Close() error                                            { return s.closeErr }
func (s *stubSession) SetReconnectCallback(cb func(*webrtc.DataChannel))       { s.reconnectCB = cb }
func (s *stubSession) SetShouldReconnect(fn func() bool)                       { s.shouldFn = fn }
func (s *stubSession) SetEndedCallback(cb func(string))                        { s.endedCB = cb }
func (s *stubSession) WatchConnection(context.Context)                         { s.watched = true }
func (s *stubSession) CanSend() bool                                           { return s.canSend }
func (s *stubSession) SubscriberCanSend() bool                                 { return s.canSend }
func (s *stubSession) GetSendQueue() chan []byte                               { return nil }
func (s *stubSession) GetBufferedAmount() uint64                               { return 0 }
func (s *stubSession) Reconnect(string)                                        {}

func registerCarrier(name string, sess engine.Session, err error) {
	enginebuiltin.Register(name, func(context.Context, enginebuiltin.Config) (engine.Session, error) {
		if err != nil {
			return nil, err
		}
		return sess, nil
	})
}

//nolint:cyclop // table-driven test naturally has many branches
func TestNewAndFeatures(t *testing.T) {
	sess := &stubSession{caps: engine.Capabilities{ByteStream: true}, canSend: true}
	registerCarrier("datachannel-test-new-and-features", sess, nil)

	tr, err := New(context.Background(), transport.Config{Carrier: "datachannel-test-new-and-features"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if !sess.connectCalled {
		t.Fatal("Connect() was not forwarded")
	}
	if err := tr.Send([]byte("payload")); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if string(sess.sent) != "payload" {
		t.Fatalf("Send() forwarded %q, want payload", sess.sent)
	}
	tr.SetReconnectCallback(func() {})
	tr.SetShouldReconnect(func() bool { return true })
	tr.SetEndedCallback(func(string) {})
	tr.WatchConnection(context.Background())
	if sess.reconnectCB == nil || sess.shouldFn == nil || sess.endedCB == nil || !sess.watched {
		t.Fatal("callbacks/watch were not forwarded")
	}
	if !tr.CanSend() {
		t.Fatal("CanSend() = false, want true")
	}

	features := tr.Features()
	if !features.Reliable || !features.Ordered || !features.MessageOriented || features.MaxPayloadSize != defaultMaxPayloadSize { //nolint:lll // long test description
		t.Fatalf("Features() = %+v", features)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNewErrorPaths(t *testing.T) {
	registerCarrier("datachannel-fail-create", nil, errDCBoom)
	_, err := New(context.Background(), transport.Config{Carrier: "datachannel-fail-create"})
	if err == nil || err.Error() != "open engine session: boom" {
		t.Fatalf("New() error = %v", err)
	}

	nonByteStream := &stubSession{caps: engine.Capabilities{}}
	registerCarrier("datachannel-no-stream", nonByteStream, nil)
	_, err = New(context.Background(), transport.Config{Carrier: "datachannel-no-stream"})
	if !errors.Is(err, ErrByteStreamUnsupported) {
		t.Fatalf("New() error = %v, want %v", err, ErrByteStreamUnsupported)
	}
}

func TestStreamTransportWrapsErrors(t *testing.T) {
	tr := &streamTransport{session: &stubSession{
		caps:       engine.Capabilities{ByteStream: true},
		connectErr: errDCConnectBoom,
		sendErr:    errDCSendBoom,
		closeErr:   errDCCloseBoom,
	}}

	if err := tr.Connect(context.Background()); err == nil || err.Error() != "session connect: connect boom" {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := tr.Send([]byte("x")); err == nil || err.Error() != "session send: send boom" {
		t.Fatalf("Send() error = %v", err)
	}
	if err := tr.Close(); err == nil || err.Error() != "session close: close boom" {
		t.Fatalf("Close() error = %v", err)
	}
}
