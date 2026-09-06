package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/logger"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/tunnelcore"
)

func (c *Client) tunnel(
	ctx context.Context,
	conn net.Conn,
	session *smux.Session,
	targetAddr string,
	targetPort int,
) {
	stream, err := session.OpenStream()
	if err != nil {
		logger.Warnf("OpenStream failed: %v", err)
		_, _ = conn.Write(replyHostUnreachable(targetAddr))
		return
	}
	defer func() { _ = stream.Close() }()
	logger.Infof("sid=%d tunnel to %s:%d", stream.ID(), targetAddr, targetPort)
	if err := c.sendConnectRequest(stream, targetAddr, targetPort); err != nil {
		logger.Warnf("sid=%d connect failed: %v", stream.ID(), err)
		c.noteConnectFailure(err, targetAddr)
		_, _ = conn.Write(replyForConnectError(err, targetAddr))
		return
	}
	if _, err := conn.Write(replySuccess(targetAddr)); err != nil {
		return
	}
	_, _ = tunnelcore.CopyBidirectional(ctx, conn, stream)
}

func (c *Client) sendConnectRequest(stream *smux.Stream, targetAddr string, targetPort int) error {
	request, err := json.Marshal(map[string]any{
		"cmd": "connect", "addr": targetAddr, "port": targetPort,
	})
	if err != nil {
		return fmt.Errorf("sid=%d marshal connect req: %w", stream.ID(), err)
	}
	_ = stream.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := stream.Write(request); err != nil {
		return fmt.Errorf("sid=%d write connect req: %w", stream.ID(), err)
	}
	_ = stream.SetWriteDeadline(time.Time{})
	ack := make([]byte, 1)
	_ = stream.SetReadDeadline(time.Now().Add(runtime.ConnectAckTimeout(c.ln)))
	if _, err := io.ReadFull(stream, ack); err != nil {
		return fmt.Errorf("sid=%d: %w (read_err=%w)", stream.ID(), ErrRemoteNotReady, err)
	}
	_ = stream.SetReadDeadline(time.Time{})
	if ack[0] != tunnelcore.ConnectAckOK {
		return &connectAckError{code: ack[0], streamID: stream.ID()}
	}
	return nil
}

// noteConnectFailure latches the exit's lack of IPv6 the first time it refuses
// an IPv6 literal as unreachable, so the rest of the session stops spending
// tunnel streams on address family the exit cannot route at all.
func (c *Client) noteConnectFailure(err error, targetAddr string) {
	var ackErr *connectAckError
	if !errors.As(err, &ackErr) || ackErr.code != socksRepHostUnreachable {
		return
	}
	if !isIPv6Literal(targetAddr) {
		return
	}
	if c.peerNoIPv6.CompareAndSwap(false, true) {
		logger.Infof("exit reports no IPv6 route (%s unreachable) - refusing further IPv6 targets locally", targetAddr)
	}
}

type connectAckError struct {
	code     byte
	streamID uint32
}

func (e *connectAckError) Error() string {
	return fmt.Sprintf("sid=%d: %s (connect ack=0x%02x)", e.streamID, ErrRemoteNotReady, e.code)
}

func (e *connectAckError) Unwrap() error { return ErrRemoteNotReady }

func replyForConnectError(err error, target string) []byte {
	var ackErr *connectAckError
	if errors.As(err, &ackErr) {
		return socks5Reply(ackErr.code, target)
	}
	return replyHostUnreachable(target)
}
