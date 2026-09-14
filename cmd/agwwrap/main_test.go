package main

import (
	"errors"
	"io"
	"syscall"
	"testing"

	"github.com/chrissnell/graywolf/pkg/agw"
)

// TestIsSessionFrame reproduces the "Received frame for unknown session"
// bug: tncd's 'X' register-ack frame echoes our own callsign in CallFrom,
// which the dispatcher previously mistook for connected-mode session
// traffic and answered with a spurious 'd' disconnect back to tncd.
func TestIsSessionFrame(t *testing.T) {
	cases := []struct {
		name string
		kind byte
		want bool
	}{
		{"connected data", KindConnectedData, true},
		{"disconnect", KindDisconnect, true},
		{"connect", KindConnect, false},
		{"register ack", 'X', false},
		{"version reply", 'R', false},
		{"port info reply", 'G', false},
		{"port caps reply", 'g', false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isSessionFrame(tc.kind)
			if got != tc.want {
				t.Errorf("isSessionFrame(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestKeepaliveHeader confirms the keepalive frame is a plain version
// request (isSessionFrame(agw.KindVersion) is false, so the dispatcher
// never routes the server's reply into a session), carrying our callsign
// and the configured radio port.
func TestKeepaliveHeader(t *testing.T) {
	hdr := keepaliveHeader(3, "N0CALL")

	if hdr.DataKind != agw.KindVersion {
		t.Errorf("DataKind = %q, want %q", hdr.DataKind, agw.KindVersion)
	}
	if hdr.Port != 3 {
		t.Errorf("Port = %d, want 3", hdr.Port)
	}
	if hdr.CallFrom != "N0CALL" {
		t.Errorf("CallFrom = %q, want %q", hdr.CallFrom, "N0CALL")
	}
	if isSessionFrame(hdr.DataKind) {
		t.Errorf("isSessionFrame(%q) = true, want false", hdr.DataKind)
	}
}

func TestIsCommandStreamClosed(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		usePty bool
		want   bool
	}{
		{"EOF without pty", io.EOF, false, true},
		{"EOF with pty", io.EOF, true, true},
		{"EIO with pty", &pathErrorWrapping{syscall.EIO}, true, true},
		{"EIO without pty", &pathErrorWrapping{syscall.EIO}, false, false},
		{"other error with pty", errors.New("boom"), true, false},
		{"other error without pty", errors.New("boom"), false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isCommandStreamClosed(tc.err, tc.usePty)
			if got != tc.want {
				t.Errorf("isCommandStreamClosed(%v, %v) = %v, want %v", tc.err, tc.usePty, got, tc.want)
			}
		})
	}
}

// pathErrorWrapping mimics the *fs.PathError wrapping that os.File.Read
// produces around a syscall errno, so errors.Is(err, syscall.EIO) works the
// same way it does with a real read error.
type pathErrorWrapping struct {
	errno syscall.Errno
}

func (e *pathErrorWrapping) Error() string { return e.errno.Error() }
func (e *pathErrorWrapping) Unwrap() error { return e.errno }

// TestPTYMasterReadAfterSlaveClose reproduces the real-world condition:
// once every open fd on a PTY's slave side is closed, a read on the master
// returns EIO rather than EOF. This confirms isCommandStreamClosed treats
// that as a normal end-of-stream when usePty is true.
func TestPTYMasterReadAfterSlaveClose(t *testing.T) {
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("openPTY unavailable in this environment: %v", err)
	}
	defer master.Close()

	if err := slave.Close(); err != nil {
		t.Fatalf("closing slave: %v", err)
	}

	buf := make([]byte, 16)
	_, readErr := master.Read(buf)
	if readErr == nil {
		t.Fatalf("expected an error reading master after slave close, got nil")
	}
	if !errors.Is(readErr, syscall.EIO) {
		t.Fatalf("expected EIO, got: %v", readErr)
	}
	if !isCommandStreamClosed(readErr, true) {
		t.Errorf("isCommandStreamClosed(%v, true) = false, want true", readErr)
	}
	if isCommandStreamClosed(readErr, false) {
		t.Errorf("isCommandStreamClosed(%v, false) = true, want false", readErr)
	}
}
