package tunnelcore

import (
	"errors"
	"fmt"
	"io"

	"github.com/xtaci/smux"

	"github.com/openlibrecommunity/olcrtc/internal/muxconn"
	"github.com/openlibrecommunity/olcrtc/internal/runtime"
	"github.com/openlibrecommunity/olcrtc/internal/transport"
)

// ErrUnknownSessionRole is returned for an unsupported smux constructor role.
var ErrUnknownSessionRole = errors.New("unknown smux role")

// SessionRole selects the matching smux constructor.
type SessionRole uint8

const (
	// ClientRole creates smux client sessions.
	ClientRole SessionRole = iota + 1
	// ServerRole creates smux server sessions.
	ServerRole
)

// SessionPair owns the data session and its optional isolated control session.
type SessionPair struct {
	DataConn       *muxconn.Conn
	DataSession    *smux.Session
	ControlConn    *muxconn.Conn
	ControlSession *smux.Session
}

// NewSessionPair builds data and optional isolated-control muxconn/smux sessions.
// If only the control session fails, the usable data pair is returned with the error.
func NewSessionPair(tr transport.Transport, keys interface {
	SealInto([]byte, []byte, []byte) ([]byte, error)
	OpenInto([]byte, []byte, []byte) ([]byte, error)
}, role SessionRole) (*SessionPair, error) {
	dataConn := muxconn.New(tr, keys)
	controlConn := muxconn.NewControl(tr, keys)
	return NewSessionPairWithConns(tr, dataConn, controlConn, role)
}

// NewSessionPairWithConns builds a pair from muxconns already installed by the caller.
func NewSessionPairWithConns(
	tr transport.Transport,
	dataConn, controlConn *muxconn.Conn,
	role SessionRole,
) (*SessionPair, error) {
	dataSession, err := NewSession(dataConn, role, runtime.SmuxConfigFor(tr))
	if err != nil {
		_ = dataConn.Close()
		if controlConn != nil {
			_ = controlConn.Close()
		}
		return nil, fmt.Errorf("data smux session: %w", err)
	}

	pair := &SessionPair{
		DataConn:       dataConn,
		DataSession:    dataSession,
		ControlSession: dataSession,
	}
	if controlConn == nil {
		return pair, nil
	}
	pair.ControlConn = controlConn
	controlSession, err := NewSession(controlConn, role, runtime.ControlSmuxConfig(runtime.MaxPayload(tr)))
	if err != nil {
		_ = controlConn.Close()
		pair.ControlConn = nil
		return pair, fmt.Errorf("control smux session: %w", err)
	}
	pair.ControlSession = controlSession
	return pair, nil
}

// NewControlSession builds only an isolated control muxconn/smux session.
func NewControlSession(
	tr transport.Transport,
	keys interface {
		SealInto([]byte, []byte, []byte) ([]byte, error)
		OpenInto([]byte, []byte, []byte) ([]byte, error)
	},
	role SessionRole,
) (*muxconn.Conn, *smux.Session, error) {
	conn := muxconn.NewControl(tr, keys)
	if conn == nil {
		return nil, nil, nil
	}
	session, err := NewSession(conn, role, runtime.ControlSmuxConfig(runtime.MaxPayload(tr)))
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("control smux session: %w", err)
	}
	return conn, session, nil
}

// NewSession creates one smux session with the constructor selected by role.
func NewSession(conn io.ReadWriteCloser, role SessionRole, cfg *smux.Config) (*smux.Session, error) {
	switch role {
	case ClientRole:
		session, err := smux.Client(conn, cfg)
		if err != nil {
			return nil, fmt.Errorf("smux client: %w", err)
		}
		return session, nil
	case ServerRole:
		session, err := smux.Server(conn, cfg)
		if err != nil {
			return nil, fmt.Errorf("smux server: %w", err)
		}
		return session, nil
	default:
		return nil, fmt.Errorf("%w: %d", ErrUnknownSessionRole, role)
	}
}

// HasIsolatedControl reports whether control uses a distinct muxconn and session.
func (p *SessionPair) HasIsolatedControl() bool {
	return p != nil && p.ControlConn != nil
}

// CloseConns closes data then control muxconns so late transport frames are discarded.
func (p *SessionPair) CloseConns() error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.DataConn != nil {
		errs = append(errs, p.DataConn.Close())
	}
	if p.ControlConn != nil {
		errs = append(errs, p.ControlConn.Close())
	}
	return errors.Join(errs...)
}

// Close deterministically closes sessions before their muxconns and returns all errors.
func (p *SessionPair) Close() error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.DataSession != nil {
		errs = append(errs, p.DataSession.Close())
	}
	if p.ControlSession != nil && p.ControlSession != p.DataSession {
		errs = append(errs, p.ControlSession.Close())
	}
	if err := p.CloseConns(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
