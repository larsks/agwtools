package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"

	"github.com/larsks/agwtools/internal/agwconn"
	"github.com/larsks/agwtools/internal/configtest"
)

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

// recordingWriter collects the headers passed to writeAGW so tests can
// assert which AGWPE frames a session emitted.
type recordingWriter struct {
	mu      sync.Mutex
	headers []*agw.Header
}

func (w *recordingWriter) write(hdr *agw.Header, _ []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.headers = append(w.headers, hdr)
	return nil
}

func (w *recordingWriter) sawDisconnect() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, hdr := range w.headers {
		if hdr.DataKind == agwconn.KindDisconnect {
			return true
		}
	}
	return false
}

// TestHandleSessionIdleTimeout confirms a session with no traffic from the
// remote station is disconnected once cfg.IdleTimeout elapses, and that the
// dispatcher is notified so it can clean up.
func TestHandleSessionIdleTimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &recordingWriter{}
	cfg := SessionConfig{
		RemoteCall:  "N0CALL",
		Callsign:    "MYCALL",
		CmdName:     "cat",
		IdleTimeout: 50 * time.Millisecond,
	}

	frames := make(chan agwconn.Frame)
	done := make(chan string, 1)

	go handleSession(ctx, w.write, cfg, frames, done)

	select {
	case remoteCall := <-done:
		if remoteCall != cfg.RemoteCall {
			t.Errorf("done callsign = %q, want %q", remoteCall, cfg.RemoteCall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleSession did not finish after idle timeout")
	}

	if !w.sawDisconnect() {
		t.Errorf("expected a disconnect frame after the idle timeout, got none")
	}
}

// TestHandleSessionIdleTimeoutResetByActivity confirms that traffic from the
// remote station resets the idle timer, so a session stays open as long as
// it keeps receiving data, and only idles out once that traffic stops.
func TestHandleSessionIdleTimeoutResetByActivity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &recordingWriter{}
	idleTimeout := 100 * time.Millisecond
	cfg := SessionConfig{
		RemoteCall:  "N0CALL",
		Callsign:    "MYCALL",
		CmdName:     "cat",
		IdleTimeout: idleTimeout,
	}

	frames := make(chan agwconn.Frame)
	done := make(chan string, 1)

	go handleSession(ctx, w.write, cfg, frames, done)

	// Send data more often than the idle timeout for longer than the
	// timeout itself; the session must still be alive afterward.
	activityWindow := idleTimeout * 3
	deadline := time.Now().Add(activityWindow)
	for time.Now().Before(deadline) {
		select {
		case frames <- agwconn.Frame{Hdr: &agw.Header{DataKind: agwconn.KindConnectedData, CallFrom: cfg.RemoteCall}, Data: []byte("x")}:
		case <-done:
			t.Fatal("session ended early despite ongoing activity")
		}
		time.Sleep(idleTimeout / 4)
	}

	select {
	case <-done:
		t.Fatal("session ended early despite ongoing activity")
	default:
	}

	// Now stop sending traffic; the session should idle out.
	select {
	case remoteCall := <-done:
		if remoteCall != cfg.RemoteCall {
			t.Errorf("done callsign = %q, want %q", remoteCall, cfg.RemoteCall)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handleSession did not finish after activity stopped")
	}
}

// TestCRLFToCR checks the translation, including a \r\n pair split across
// two reads, which is the case a naive per-chunk replace would get wrong.
func TestCRLFToCR(t *testing.T) {
	cases := []struct {
		name   string
		chunks []string
		want   string
	}{
		{"no line endings", []string{"hello"}, "hello"},
		{"crlf", []string{"a\r\nb\r\n"}, "a\rb\r"},
		{"bare lf untouched", []string{"a\nb"}, "a\nb"},
		{"bare cr untouched", []string{"a\rb"}, "a\rb"},
		{"cr cr lf", []string{"a\r\r\nb"}, "a\r\rb"},
		{"lf cr lf", []string{"a\n\r\nb"}, "a\n\rb"},
		{"split across chunks", []string{"a\r", "\nb"}, "a\rb"},
		{"chunk of only lf after cr", []string{"a\r", "\n", "b"}, "a\rb"},
		{"cr at end of chunk emitted immediately", []string{"a\r"}, "a\r"},
		{"lf after non-cr chunk", []string{"a", "\nb"}, "a\nb"},
		{"empty chunk keeps state", []string{"a\r", "", "\nb"}, "a\rb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var f crlfToCR
			var got []byte
			for _, chunk := range tc.chunks {
				buf := []byte(chunk)
				n := f.filter(buf)
				got = append(got, buf[:n]...)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// dataWriter collects the payload of connected-data frames so tests can
// assert exactly which bytes a session sent to the remote station.
type dataWriter struct {
	mu   sync.Mutex
	data []byte
}

func (w *dataWriter) write(hdr *agw.Header, data []byte) error {
	if hdr.DataKind != agwconn.KindConnectedData {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.data = append(w.data, data...)
	return nil
}

func (w *dataWriter) len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.data)
}

func (w *dataWriter) string() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.data)
}

// runEchoSession feeds input through a `cat` session and returns what was
// sent back to the remote station once wantLen bytes have arrived.
func runEchoSession(t *testing.T, crlfToCR bool, input string, wantLen int) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := &dataWriter{}
	cfg := SessionConfig{
		RemoteCall: "N0CALL",
		Callsign:   "MYCALL",
		CmdName:    "cat",
		CRLFToCR:   crlfToCR,
	}

	frames := make(chan agwconn.Frame)
	done := make(chan string, 1)
	go handleSession(ctx, w.write, cfg, frames, done)

	frames <- agwconn.Frame{Hdr: &agw.Header{DataKind: agwconn.KindConnectedData, CallFrom: cfg.RemoteCall}, Data: []byte(input)}

	deadline := time.Now().Add(2 * time.Second)
	for w.len() < wantLen && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleSession did not finish")
	}
	return w.string()
}

// TestHandleSessionCRLFToCR confirms that with CRLFToCR set, command output
// reaches the remote station with \r\n rewritten to \r.
func TestHandleSessionCRLFToCR(t *testing.T) {
	got := runEchoSession(t, true, "one\r\ntwo\r\n", len("one\rtwo\r"))
	if want := "one\rtwo\r"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestHandleSessionCRLFPassthrough confirms --raw leaves output alone.
func TestHandleSessionCRLFPassthrough(t *testing.T) {
	got := runEchoSession(t, false, "one\r\ntwo\r\n", len("one\r\ntwo\r\n"))
	if want := "one\r\ntwo\r\n"; got != want {
		t.Errorf("got %q, want %q", got, want)
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

// TestExampleConfig loads config.example.toml, with every option enabled,
// through agwlisten's real option set. That fails if the example puts an option
// in a section it isn't valid in, or names one agwlisten doesn't have, and it
// also fails if an option is missing from the example.
func TestExampleConfig(t *testing.T) {
	path := configtest.UncommentedExample(t)

	if err := flag.CommandLine.Set("config", path); err != nil {
		t.Fatal(err)
	}
	loaded, err := agwconn.LoadConfigFile(flag.CommandLine, "agwlisten")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != path {
		t.Errorf("loaded %q, want %q", loaded, path)
	}

	configtest.RequireCovers(t, flag.CommandLine, "agwlisten", path)
}
