package agwconn

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"

	"github.com/larsks/agwtools/internal/fakeagw"
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
			got := IsSessionFrame(tc.kind)
			if got != tc.want {
				t.Errorf("IsSessionFrame(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestKeepaliveHeader confirms the keepalive frame is a plain version
// request (IsSessionFrame(agw.KindVersion) is false, so the dispatcher
// never routes the server's reply into a session), carrying our callsign
// and the configured radio port.
func TestKeepaliveHeader(t *testing.T) {
	hdr := KeepaliveHeader(3, "N0CALL")

	if hdr.DataKind != agw.KindVersion {
		t.Errorf("DataKind = %q, want %q", hdr.DataKind, agw.KindVersion)
	}
	if hdr.Port != 3 {
		t.Errorf("Port = %d, want 3", hdr.Port)
	}
	if hdr.CallFrom != "N0CALL" {
		t.Errorf("CallFrom = %q, want %q", hdr.CallFrom, "N0CALL")
	}
	if IsSessionFrame(hdr.DataKind) {
		t.Errorf("IsSessionFrame(%q) = true, want false", hdr.DataKind)
	}
}

func TestAddFlagsDefaults(t *testing.T) {
	var c Config
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	c.AddFlags(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}

	if c.HostPort != "localhost:8000" {
		t.Errorf("HostPort = %q", c.HostPort)
	}
	if c.Callsign != "NOCALL" {
		t.Errorf("Callsign = %q", c.Callsign)
	}
	if c.RadioPort != 0 {
		t.Errorf("RadioPort = %d", c.RadioPort)
	}
	if c.KeepAlive != 60*time.Second {
		t.Errorf("KeepAlive = %s", c.KeepAlive)
	}
}

// TestAddFlagsShorthands confirms the short and long spellings agree, and in
// particular that -h means --host rather than help.
func TestAddFlagsShorthands(t *testing.T) {
	cases := [][]string{
		{"-h", "gw:9000", "-c", "N0CALL", "-p", "2", "-k", "5s"},
		{"--host", "gw:9000", "--callsign", "N0CALL", "--port", "2", "--keepalive", "5s"},
	}
	for _, args := range cases {
		var c Config
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		c.AddFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		want := Config{HostPort: "gw:9000", Callsign: "N0CALL", RadioPort: 2, KeepAlive: 5 * time.Second}
		if c != want {
			t.Errorf("parse %v: got %+v, want %+v", args, c, want)
		}
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"ok", Config{RadioPort: 0}, false},
		{"max port", Config{RadioPort: 255}, false},
		{"port too large", Config{RadioPort: 256}, true},
		{"negative port", Config{RadioPort: -1}, true},
		{"negative keepalive", Config{KeepAlive: -time.Second}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func dial(t *testing.T, srv *fakeagw.Server, keepalive time.Duration) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), Config{
		HostPort:  srv.Addr(),
		Callsign:  "MYCALL",
		RadioPort: 3,
		KeepAlive: keepalive,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestDialRegistersCallsign(t *testing.T) {
	srv := fakeagw.New(t)
	dial(t, srv, 0)

	f := srv.Recv()
	if f.Hdr.DataKind != agw.KindRegisterCallsign {
		t.Errorf("DataKind = %q, want %q", f.Hdr.DataKind, agw.KindRegisterCallsign)
	}
	if f.Hdr.CallFrom != "MYCALL" {
		t.Errorf("CallFrom = %q, want MYCALL", f.Hdr.CallFrom)
	}
	if f.Hdr.Port != 3 {
		t.Errorf("Port = %d, want 3", f.Hdr.Port)
	}
}

func TestDialRegistrationRefused(t *testing.T) {
	srv := fakeagw.New(t)
	srv.SetRegistration(fakeagw.Reject)

	_, err := Dial(context.Background(), Config{HostPort: srv.Addr(), Callsign: "MYCALL"})
	if err == nil {
		t.Fatal("Dial succeeded although the gateway refused the registration")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("error %q does not mention the refusal", err)
	}
}

func TestDialRegistrationNoReply(t *testing.T) {
	old := registerTimeout
	registerTimeout = 100 * time.Millisecond
	t.Cleanup(func() { registerTimeout = old })

	srv := fakeagw.New(t)
	srv.SetRegistration(fakeagw.Ignore)

	if _, err := Dial(context.Background(), Config{HostPort: srv.Addr(), Callsign: "MYCALL"}); err == nil {
		t.Fatal("Dial succeeded although the gateway never acknowledged the registration")
	}
}

func TestDialCanceledWhileRegistering(t *testing.T) {
	srv := fakeagw.New(t)
	srv.SetRegistration(fakeagw.Ignore)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := Dial(ctx, Config{HostPort: srv.Addr(), Callsign: "MYCALL"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Dial error = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Errorf("Dial took %s to notice cancellation", time.Since(start))
	}
}

func TestDialFailure(t *testing.T) {
	// Grab a free port, then stop listening on it so the dial is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	if _, err := Dial(context.Background(), Config{HostPort: addr}); err == nil {
		t.Errorf("Dial to closed port %s succeeded", addr)
	}
}

func TestDialCanceledContext(t *testing.T) {
	srv := fakeagw.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Dial(ctx, Config{HostPort: srv.Addr()}); err == nil {
		t.Error("Dial with canceled context succeeded")
	}
}

func TestFramesDelivered(t *testing.T) {
	srv := fakeagw.New(t)
	c := dial(t, srv, 0)

	srv.Send(&agw.Header{DataKind: KindConnectedData, CallFrom: "REMOTE", CallTo: "MYCALL"}, []byte("hello"))

	select {
	case f := <-c.Frames():
		if f.Hdr.DataKind != KindConnectedData || string(f.Data) != "hello" || f.Hdr.CallFrom != "REMOTE" {
			t.Errorf("unexpected frame %+v %q", f.Hdr, f.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no frame delivered")
	}
}

func TestConnectionDropReported(t *testing.T) {
	srv := fakeagw.New(t)
	c := dial(t, srv, 0)
	srv.Recv() // registration, so the client is definitely connected

	srv.Close()

	select {
	case <-c.Done():
		if c.Err() == nil {
			t.Error("Err() = nil after connection drop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after connection drop")
	}
}

func TestKeepalive(t *testing.T) {
	srv := fakeagw.New(t)
	dial(t, srv, 20*time.Millisecond)
	srv.Recv() // registration

	f := srv.RecvKind(agw.KindVersion)
	if f.Hdr.CallFrom != "MYCALL" || f.Hdr.Port != 3 {
		t.Errorf("keepalive header = %+v", f.Hdr)
	}
}

func TestKeepaliveDisabled(t *testing.T) {
	srv := fakeagw.New(t)
	dial(t, srv, 0)
	srv.Recv() // registration

	if f, ok := srv.TryRecv(150 * time.Millisecond); ok {
		t.Errorf("unexpected frame %q with keepalive disabled", f.Hdr.DataKind)
	}
}

// TestConcurrentWritesDoNotInterleave hammers Write from many goroutines and
// checks the server decodes every frame intact.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	srv := fakeagw.New(t)
	c := dial(t, srv, 0)
	srv.Recv() // registration

	const writers, perWriter = 8, 25
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = 'x'
	}

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				if err := c.Write(&agw.Header{DataKind: KindConnectedData, CallFrom: "MYCALL", CallTo: "REMOTE"}, payload); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}()
	}

	for range writers * perWriter {
		f := srv.Recv()
		if f.Hdr.DataKind != KindConnectedData || len(f.Data) != len(payload) {
			t.Fatalf("corrupt frame: kind %q, %d bytes", f.Hdr.DataKind, len(f.Data))
		}
	}
	wg.Wait()
}

func TestCloseIsIdempotentAndWriteFailsAfter(t *testing.T) {
	srv := fakeagw.New(t)
	c := dial(t, srv, 10*time.Millisecond)

	c.Close()
	c.Close()

	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done not closed after Close")
	}
	if err := c.Write(&agw.Header{DataKind: KindConnectedData}, nil); err == nil {
		t.Error("Write after Close succeeded")
	}
}
