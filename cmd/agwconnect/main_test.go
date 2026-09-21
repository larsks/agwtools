package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"

	"github.com/larsks/agwtools/internal/agwconn"
	"github.com/larsks/agwtools/internal/fakeagw"
)

func TestEOLFilter(t *testing.T) {
	cases := []struct {
		name   string
		out    byte
		chunks []string
		want   string
	}{
		{"to CR: lf", '\r', []string{"a\nb\n"}, "a\rb\r"},
		{"to CR: cr untouched", '\r', []string{"a\rb"}, "a\rb"},
		{"to CR: crlf collapses", '\r', []string{"a\r\nb"}, "a\rb"},
		{"to CR: double lf keeps both", '\r', []string{"a\n\nb"}, "a\r\rb"},
		{"to CR: crlf split across chunks", '\r', []string{"a\r", "\nb"}, "a\rb"},
		{"to CR: lf after lf chunk", '\r', []string{"a\n", "\nb"}, "a\r\rb"},
		{"to LF: cr", '\n', []string{"a\rb\r"}, "a\nb\n"},
		{"to LF: crlf collapses", '\n', []string{"a\r\nb\r\n"}, "a\nb\n"},
		{"to LF: lf untouched", '\n', []string{"a\nb"}, "a\nb"},
		{"to LF: cr cr is two lines", '\n', []string{"a\r\rb"}, "a\n\nb"},
		{"to LF: split crlf", '\n', []string{"a\r", "\nb"}, "a\nb"},
		{"to LF: empty chunk keeps state", '\n', []string{"a\r", "", "\nb"}, "a\nb"},
		{"no line endings", '\n', []string{"hello"}, "hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := eolFilter{out: tc.out}
			var got []byte
			for _, chunk := range tc.chunks {
				buf := []byte(chunk)
				got = append(got, buf[:f.filter(buf)]...)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestViaPayload(t *testing.T) {
	got := viaPayload([]string{"WIDE1-1", "WIDE2-1"})
	want := []byte("\x02WIDE1-1\x00\x00\x00WIDE2-1\x00\x00\x00")
	if !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPrepare(t *testing.T) {
	base := func() Options {
		return Options{Config: agwconn.Config{RadioPort: 0}}
	}
	nineVias := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I"}
	eightVias := nineVias[:8]

	cases := []struct {
		name    string
		mod     func(*Options)
		args    []string
		want    string
		wantErr bool
	}{
		{"ok", nil, []string{"n0call-1"}, "N0CALL-1", false},
		{"no args", nil, nil, "", true},
		{"two args", nil, []string{"A", "B"}, "", true},
		{"port too large", func(o *Options) { o.RadioPort = 256 }, []string{"A"}, "", true},
		{"negative wait", func(o *Options) { o.wait = -1 }, []string{"A"}, "", true},
		{"remote too long", nil, []string{"N0CALL-12345"}, "", true},
		{"remote has space", nil, []string{"N0 CALL"}, "", true},
		{"eight digis ok", func(o *Options) { o.via = append([]string(nil), eightVias...) }, []string{"A"}, "A", false},
		{"nine digis", func(o *Options) { o.via = append([]string(nil), nineVias...) }, []string{"A"}, "", true},
		{"bad digi", func(o *Options) { o.via = []string{"WIDE 1"} }, []string{"A"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := base()
			if tc.mod != nil {
				tc.mod(&o)
			}
			got, err := o.prepare(tc.args, false, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("prepare() error = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("remote = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveWait covers the --wait defaults: a terminal disconnects at end
// of input (Ctrl-D), piped input waits for replies, and an explicit --wait
// overrides both, including --wait=0 on a pipe.
func TestResolveWait(t *testing.T) {
	cases := []struct {
		name     string
		seconds  int
		set      bool
		terminal bool
		want     time.Duration
	}{
		{"terminal, unset", 0, false, true, 0},
		{"pipe, unset", 0, false, false, 30 * time.Second},
		{"terminal, explicit", 10, true, true, 10 * time.Second},
		{"pipe, explicit", 5, true, false, 5 * time.Second},
		{"pipe, explicit zero", 0, true, false, 0},
		{"terminal, explicit zero", 0, true, true, 0},
		{"unset ignores the value", 99, false, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveWait(tc.seconds, tc.set, tc.terminal); got != tc.want {
				t.Errorf("resolveWait(%d, %v, %v) = %s, want %s", tc.seconds, tc.set, tc.terminal, got, tc.want)
			}
		})
	}
}

// TestPrepareSetsEOFTimeout confirms prepare wires --wait through to the
// timeout run() uses.
func TestPrepareSetsEOFTimeout(t *testing.T) {
	o := Options{wait: 7}
	if _, err := o.prepare([]string{"A"}, true, true); err != nil {
		t.Fatal(err)
	}
	if o.eofTimeout != 7*time.Second {
		t.Errorf("eofTimeout = %s, want 7s", o.eofTimeout)
	}

	o = Options{}
	if _, err := o.prepare([]string{"A"}, false, true); err != nil {
		t.Fatal(err)
	}
	if o.eofTimeout != 0 {
		t.Errorf("eofTimeout on a terminal with no --wait = %s, want 0", o.eofTimeout)
	}
}

func TestPrepareUppercasesVia(t *testing.T) {
	o := Options{via: []string{"wide1-1", " wide2-1 "}}
	if _, err := o.prepare([]string{"n0call"}, false, false); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(o.via, ","); got != "WIDE1-1,WIDE2-1" {
		t.Errorf("via = %q", got)
	}
}

// TestFlags confirms the command line matches the requested interface,
// including the shorthands shared with agwlisten.
func TestFlags(t *testing.T) {
	for name, short := range map[string]string{
		"host": "h", "port": "p", "callsign": "c", "keepalive": "k", "via": "v", "raw": "r", "wait": "w",
	} {
		f := flag.CommandLine.Lookup(name)
		if f == nil {
			t.Errorf("--%s is not defined", name)
			continue
		}
		if f.Shorthand != short {
			t.Errorf("--%s shorthand = %q, want %q", name, f.Shorthand, short)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness runs agwconnect's run() against a fake gateway.
type harness struct {
	t      *testing.T
	srv    *fakeagw.Server
	stdin  *io.PipeWriter
	stdout *syncBuffer
	stderr *syncBuffer
	cancel context.CancelFunc
	result chan error
	exited chan struct{} // closed once run() has returned
}

const (
	myCall = "MYCALL"
	remote = "REMOTE"
)

func start(t *testing.T, mod func(*Options)) *harness {
	t.Helper()

	srv := fakeagw.New(t)
	opts := Options{
		Config:     agwconn.Config{HostPort: srv.Addr(), Callsign: myCall, RadioPort: 2},
		eofTimeout: 5 * time.Second,
	}
	if mod != nil {
		mod(&opts)
	}

	pr, pw := io.Pipe()
	t.Cleanup(func() { pw.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h := &harness{
		t: t, srv: srv, stdin: pw, cancel: cancel,
		stdout: &syncBuffer{}, stderr: &syncBuffer{},
		result: make(chan error, 1),
		exited: make(chan struct{}),
	}
	go func() {
		h.result <- run(ctx, opts, remote, pr, h.stdout, h.stderr)
		close(h.exited)
	}()

	// Don't leave run() going once the test is over: it would outlive the
	// test's use of package variables such as disconnectWait.
	t.Cleanup(func() {
		cancel()
		srv.Close()
		select {
		case <-h.exited:
		case <-time.After(3 * time.Second):
			t.Error("run() did not exit at the end of the test")
		}
	})
	return h
}

// connect answers the connect request the way a gateway does when the link
// comes up, returning the request frame.
func (h *harness) connect() fakeagw.Frame {
	h.t.Helper()
	var req fakeagw.Frame
	for {
		req = h.srv.Recv()
		if req.Hdr.DataKind == agwconn.KindConnect || req.Hdr.DataKind == agwconn.KindConnectVia {
			break
		}
	}
	h.remoteSends(agwconn.KindConnect, "*** CONNECTED With "+remote+"\r\n")

	// Wait until run() has processed the ack, so a test that goes on to
	// interrupt the session interrupts an established link, not a pending
	// connect.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.stderr.String(), "Connected to") {
		if time.Now().After(deadline) {
			h.t.Fatal("run() never reported the connection")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return req
}

func (h *harness) remoteSends(kind byte, data string) {
	h.t.Helper()
	h.srv.Send(&agw.Header{Port: 2, DataKind: kind, PID: 0xF0, CallFrom: remote, CallTo: myCall}, []byte(data))
}

func (h *harness) wait() error {
	h.t.Helper()
	select {
	case err := <-h.result:
		return err
	case <-time.After(3 * time.Second):
		h.t.Fatal("run did not finish")
		return nil
	}
}

func (h *harness) waitStdout(want string) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(h.stdout.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("stdout %q never contained %q", h.stdout.String(), want)
}

func TestConnectRequest(t *testing.T) {
	h := start(t, nil)
	req := h.connect()

	if req.Hdr.DataKind != agwconn.KindConnect {
		t.Errorf("DataKind = %q, want %q", req.Hdr.DataKind, agwconn.KindConnect)
	}
	if req.Hdr.CallFrom != myCall || req.Hdr.CallTo != remote {
		t.Errorf("CallFrom/CallTo = %q/%q, want %q/%q", req.Hdr.CallFrom, req.Hdr.CallTo, myCall, remote)
	}
	if req.Hdr.Port != 2 {
		t.Errorf("Port = %d, want 2", req.Hdr.Port)
	}
	if len(req.Data) != 0 {
		t.Errorf("'C' frame carried %d bytes of data", len(req.Data))
	}
}

func TestConnectVia(t *testing.T) {
	h := start(t, func(o *Options) { o.via = []string{"WIDE1-1", "WIDE2-1"} })
	req := h.connect()

	if req.Hdr.DataKind != agwconn.KindConnectVia {
		t.Fatalf("DataKind = %q, want %q", req.Hdr.DataKind, agwconn.KindConnectVia)
	}
	if want := viaPayload([]string{"WIDE1-1", "WIDE2-1"}); !bytes.Equal(req.Data, want) {
		t.Errorf("payload = %q, want %q", req.Data, want)
	}
	if req.Hdr.CallFrom != myCall || req.Hdr.CallTo != remote {
		t.Errorf("CallFrom/CallTo = %q/%q", req.Hdr.CallFrom, req.Hdr.CallTo)
	}
}

func TestConnectRefused(t *testing.T) {
	h := start(t, nil)
	h.srv.RecvKind(agwconn.KindConnect)
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED With REMOTE: channel 0 is APRS-only\r\n")

	err := h.wait()
	if err == nil || !strings.Contains(err.Error(), "APRS-only") {
		t.Errorf("error = %v, want one carrying the gateway's reason", err)
	}
}

// TestDataBothWays covers the whole exchange: stdin becomes 'D' frames with
// \n rewritten to \r, and incoming 'D' frames reach stdout with \r rewritten
// to \n. A remote disconnect then ends the run cleanly.
func TestDataBothWays(t *testing.T) {
	h := start(t, nil)
	h.connect()

	if _, err := h.stdin.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	f := h.srv.RecvKind(agwconn.KindConnectedData)
	if string(f.Data) != "hello\r" {
		t.Errorf("sent %q, want %q", f.Data, "hello\r")
	}
	if f.Hdr.CallFrom != myCall || f.Hdr.CallTo != remote || f.Hdr.Port != 2 {
		t.Errorf("data header = %+v", f.Hdr)
	}
	if f.Hdr.PID != 0xF0 {
		t.Errorf("PID = %#x, want 0xF0", f.Hdr.PID)
	}

	h.remoteSends(agwconn.KindConnectedData, "world\r")
	h.waitStdout("world\n")

	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")
	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil after remote disconnect", err)
	}
	if got := h.stdout.String(); got != "world\n" {
		t.Errorf("stdout = %q, want %q", got, "world\n")
	}
}

func TestRawModeLeavesLineEndingsAlone(t *testing.T) {
	h := start(t, func(o *Options) { o.raw = true })
	h.connect()

	h.stdin.Write([]byte("a\nb\r\n"))
	f := h.srv.RecvKind(agwconn.KindConnectedData)
	if string(f.Data) != "a\nb\r\n" {
		t.Errorf("sent %q, want unchanged", f.Data)
	}

	h.remoteSends(agwconn.KindConnectedData, "x\r\ny\r")
	h.waitStdout("y\r")
	if got := h.stdout.String(); got != "x\r\ny\r" {
		t.Errorf("stdout = %q, want unchanged", got)
	}
}

func TestLargeInputIsChunked(t *testing.T) {
	h := start(t, func(o *Options) { o.raw = true })
	h.connect()

	go h.stdin.Write(bytes.Repeat([]byte("x"), 1000))

	total := 0
	for total < 1000 {
		f := h.srv.RecvKind(agwconn.KindConnectedData)
		if len(f.Data) > agwconn.MaxDataLen {
			t.Fatalf("frame carries %d bytes, more than the %d-byte limit", len(f.Data), agwconn.MaxDataLen)
		}
		total += len(f.Data)
	}
}

func TestIgnoresOtherStations(t *testing.T) {
	h := start(t, nil)
	h.connect()

	h.srv.Send(&agw.Header{DataKind: agwconn.KindConnectedData, CallFrom: "OTHER", CallTo: myCall}, []byte("intruder\r"))
	h.srv.Send(&agw.Header{DataKind: agwconn.KindDisconnect, CallFrom: "OTHER", CallTo: myCall}, nil)
	h.remoteSends(agwconn.KindConnectedData, "real\r")
	h.waitStdout("real\n")

	select {
	case err := <-h.result:
		t.Fatalf("run ended on another station's disconnect: %v", err)
	default:
	}
	if strings.Contains(h.stdout.String(), "intruder") {
		t.Errorf("stdout %q contains another station's data", h.stdout.String())
	}
}

func TestInterruptSendsDisconnect(t *testing.T) {
	h := start(t, nil)
	h.connect()

	h.cancel()
	d := h.srv.RecvKind(agwconn.KindDisconnect)
	if d.Hdr.CallFrom != myCall || d.Hdr.CallTo != remote {
		t.Errorf("disconnect header = %+v", d.Hdr)
	}
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")

	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil after graceful interrupt", err)
	}
}

func TestInterruptWhileConnecting(t *testing.T) {
	h := start(t, nil)
	h.srv.RecvKind(agwconn.KindConnect)

	h.cancel()
	h.srv.RecvKind(agwconn.KindDisconnect)

	if err := h.wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("run() = %v, want context.Canceled", err)
	}
}

func TestDisconnectGivesUpWaiting(t *testing.T) {
	old := disconnectWait
	disconnectWait = 50 * time.Millisecond
	t.Cleanup(func() { disconnectWait = old })

	h := start(t, nil)
	h.connect()

	h.cancel()
	h.srv.RecvKind(agwconn.KindDisconnect)
	// The gateway never confirms.
	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil", err)
	}
	if !strings.Contains(h.stderr.String(), "Timed out") {
		t.Errorf("stderr %q does not mention the timeout", h.stderr.String())
	}
}

// TestEOFWaitsForRemote confirms that end of input does not disconnect right
// away: replies that arrive afterwards are still printed, and the run ends
// when the remote hangs up.
func TestEOFWaitsForRemote(t *testing.T) {
	h := start(t, nil)
	h.connect()

	h.stdin.Write([]byte("bye\n"))
	h.stdin.Close()
	h.srv.RecvKind(agwconn.KindConnectedData)

	if f, ok := h.srv.TryRecv(150 * time.Millisecond); ok {
		t.Fatalf("client sent %q after EOF instead of waiting", f.Hdr.DataKind)
	}

	h.remoteSends(agwconn.KindConnectedData, "73\r")
	h.waitStdout("73\n")
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")

	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil", err)
	}
}

func TestEOFTimeoutDisconnects(t *testing.T) {
	h := start(t, func(o *Options) { o.eofTimeout = 100 * time.Millisecond })
	h.connect()

	h.stdin.Close()
	h.srv.RecvKind(agwconn.KindDisconnect)
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")

	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil", err)
	}
}

func TestEOFTimeoutZeroDisconnectsImmediately(t *testing.T) {
	h := start(t, func(o *Options) { o.eofTimeout = 0 })
	h.connect()

	h.stdin.Close()
	h.srv.RecvKind(agwconn.KindDisconnect)
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")

	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil", err)
	}
}

// TestEOFTimeoutIsInactivityBased confirms data from the remote keeps the
// session alive after end of input, so a slow multi-part reply isn't cut off.
func TestEOFTimeoutIsInactivityBased(t *testing.T) {
	h := start(t, func(o *Options) { o.eofTimeout = 300 * time.Millisecond })
	h.connect()
	h.stdin.Close()

	// Five replies 150ms apart span 750ms, well past the timeout.
	for range 5 {
		time.Sleep(150 * time.Millisecond)
		h.remoteSends(agwconn.KindConnectedData, "part\r")
	}
	if f, ok := h.srv.TryRecv(10 * time.Millisecond); ok {
		t.Fatalf("client sent %q while replies were still arriving", f.Hdr.DataKind)
	}

	// Once the remote goes quiet the timeout applies.
	h.srv.RecvKind(agwconn.KindDisconnect)
	h.remoteSends(agwconn.KindDisconnect, "*** DISCONNECTED\r\n")
	if err := h.wait(); err != nil {
		t.Errorf("run() = %v, want nil", err)
	}
}

func TestGatewayDropEndsSession(t *testing.T) {
	h := start(t, nil)
	h.connect()

	h.srv.Close()
	if err := h.wait(); err == nil {
		t.Error("run() = nil, want an error when the gateway connection drops")
	}
}

func TestRegistrationRefusedFailsRun(t *testing.T) {
	srv := fakeagw.New(t)
	srv.SetRegistration(fakeagw.Reject)

	opts := Options{Config: agwconn.Config{HostPort: srv.Addr(), Callsign: myCall}, eofTimeout: time.Second}
	err := run(context.Background(), opts, remote, strings.NewReader(""), &syncBuffer{}, &syncBuffer{})
	if err == nil {
		t.Fatal("run() = nil although the gateway refused the registration")
	}
}
