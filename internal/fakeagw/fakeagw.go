// Package fakeagw is a minimal in-process AGWPE gateway for tests. It accepts
// a single client connection, records every frame the client sends, and lets
// the test inject frames back.
package fakeagw

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
)

// Frame is a frame received from the client.
type Frame struct {
	Hdr  *agw.Header
	Data []byte
}

// Registration selects how the gateway answers an 'X' (register callsign)
// frame.
type Registration int

const (
	Accept Registration = iota // reply 0x01 (the default)
	Reject                     // reply 0x00
	Ignore                     // send no reply
)

// Server is a fake AGWPE gateway listening on a loopback port.
type Server struct {
	t    *testing.T
	ln   net.Listener
	conn chan net.Conn
	recv chan Frame

	mu      sync.Mutex
	current net.Conn
	reg     Registration
	writeMu sync.Mutex
}

// SetRegistration chooses how 'X' frames are answered. Call it before the
// client connects.
func (s *Server) SetRegistration(r Registration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reg = r
}

// New starts a fake gateway. It is shut down when the test finishes.
func New(t *testing.T) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &Server{
		t:    t,
		ln:   ln,
		conn: make(chan net.Conn, 1),
		recv: make(chan Frame, 64),
	}
	t.Cleanup(func() {
		ln.Close()
		s.mu.Lock()
		if s.current != nil {
			s.current.Close()
		}
		s.mu.Unlock()
	})
	go s.acceptLoop()
	return s
}

// Addr is the host:port clients should dial.
func (s *Server) Addr() string {
	return s.ln.Addr().String()
}

func (s *Server) acceptLoop() {
	c, err := s.ln.Accept()
	if err != nil {
		return
	}
	s.mu.Lock()
	s.current = c
	s.mu.Unlock()
	s.conn <- c
	for {
		hdr, data, err := agw.ReadFrame(c)
		if err != nil {
			close(s.recv)
			return
		}
		s.recv <- Frame{hdr, data}
		if hdr.DataKind == agw.KindRegisterCallsign {
			s.mu.Lock()
			reg := s.reg
			s.mu.Unlock()
			if reg != Ignore {
				ack := byte(1)
				if reg == Reject {
					ack = 0
				}
				s.writeMu.Lock()
				_ = agw.WriteFrame(c, &agw.Header{DataKind: agw.KindRegisterCallsign, CallFrom: hdr.CallFrom}, []byte{ack})
				s.writeMu.Unlock()
			}
		}
	}
}

// Send writes a frame to the client, waiting for it to connect first.
func (s *Server) Send(hdr *agw.Header, data []byte) {
	s.t.Helper()
	var c net.Conn
	select {
	case c = <-s.conn:
		s.conn <- c
	case <-time.After(2 * time.Second):
		s.t.Fatal("fakeagw: no client connected")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := agw.WriteFrame(c, hdr, data); err != nil {
		s.t.Fatalf("fakeagw: send: %v", err)
	}
}

// Recv returns the next frame the client sent, failing the test if none
// arrives in time.
func (s *Server) Recv() Frame {
	s.t.Helper()
	select {
	case f, ok := <-s.recv:
		if !ok {
			s.t.Fatal("fakeagw: client closed the connection")
		}
		return f
	case <-time.After(2 * time.Second):
		s.t.Fatal("fakeagw: timed out waiting for a frame from the client")
	}
	return Frame{}
}

// TryRecv waits up to d for the next frame from the client and reports
// whether one arrived. Unlike Recv, timing out is not a failure.
func (s *Server) TryRecv(d time.Duration) (Frame, bool) {
	select {
	case f, ok := <-s.recv:
		return f, ok
	case <-time.After(d):
		return Frame{}, false
	}
}

// RecvKind returns the next frame of the given kind, discarding others
// (e.g. keepalives).
func (s *Server) RecvKind(kind byte) Frame {
	s.t.Helper()
	for {
		f := s.Recv()
		if f.Hdr.DataKind == kind {
			return f
		}
	}
}

// Close drops the client connection.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current != nil {
		s.current.Close()
	}
}
