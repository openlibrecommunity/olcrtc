// Package fakedns is a UDP name server for tests. It answers A queries for the
// names it was given and NOERROR with no records for everything else, which is
// exactly enough for a resolver under test to prove where its questions went.
package fakedns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"

	"golang.org/x/net/dns/dnsmessage"
)

// errNotIPv4 rejects a record whose address is not an IPv4 literal.
var errNotIPv4 = errors.New("not an IPv4 address")

// Server answers on Addr until Close.
type Server struct {
	Addr    string
	conn    net.PacketConn
	records map[string]net.IP
	queries atomic.Int64
	silent  bool
}

// Queries is how many questions arrived, answered or not.
func (s *Server) Queries() int { return int(s.queries.Load()) }

// StartSilent listens and reads every question, and answers none of them - a
// resolver that a network has blackholed rather than refused.
func StartSilent() (*Server, error) {
	return start(nil, true)
}

// Start listens on a loopback port and answers the given name → IPv4 pairs.
func Start(records map[string]string) (*Server, error) {
	return start(records, false)
}

// start sets every field, silence included, before the serving goroutine
// can read any of them.
func start(records map[string]string, silent bool) (*Server, error) {
	conn, err := (&net.ListenConfig{}).ListenPacket(context.Background(), "udp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	s := &Server{Addr: conn.LocalAddr().String(), conn: conn, records: map[string]net.IP{}, silent: silent}
	for name, ip := range records {
		v4 := net.ParseIP(ip).To4()
		if v4 == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s: %q: %w", name, ip, errNotIPv4)
		}
		s.records[strings.ToLower(strings.TrimSuffix(name, "."))+"."] = v4
	}
	go s.serve()
	return s, nil
}

// Close stops answering.
func (s *Server) Close() error {
	if err := s.conn.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

func (s *Server) serve() {
	buf := make([]byte, 1500)
	for {
		n, from, err := s.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		s.queries.Add(1)
		if s.silent {
			continue
		}
		if reply, ok := s.answer(buf[:n]); ok {
			_, _ = s.conn.WriteTo(reply, from)
		}
	}
}

func (s *Server) answer(query []byte) ([]byte, bool) {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil, false
	}
	q, err := p.Question()
	if err != nil {
		return nil, false
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 h.ID,
		Response:           true,
		Authoritative:      true,
		RecursionDesired:   h.RecursionDesired,
		RecursionAvailable: true,
	})
	b.EnableCompression()
	if err = b.StartQuestions(); err != nil {
		return nil, false
	}
	if err = b.Question(q); err != nil {
		return nil, false
	}
	if err = b.StartAnswers(); err != nil {
		return nil, false
	}
	if ip, ok := s.records[strings.ToLower(q.Name.String())]; ok && q.Type == dnsmessage.TypeA {
		var a [4]byte
		copy(a[:], ip)
		err = b.AResource(
			dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			dnsmessage.AResource{A: a},
		)
		if err != nil {
			return nil, false
		}
	}
	out, err := b.Finish()
	return out, err == nil
}
