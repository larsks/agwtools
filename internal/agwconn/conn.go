package agwconn

import (
	"context"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
)

// Connected-mode data kinds. Which side is in CallFrom depends on direction:
// on frames we send it is our callsign, on frames we receive it is the
// remote station's.
const (
	KindConnect       byte = 'C'
	KindConnectVia    byte = 'v'
	KindDisconnect    byte = 'd'
	KindConnectedData byte = 'D'
)

// MaxDataLen is the largest payload to put in a single 'D' frame. The AGWPE
// API document recommends keeping connected-mode data to 255 bytes or less.
const MaxDataLen = 255

// registerTimeout bounds how long Dial waits for the gateway to acknowledge
// callsign registration.
var registerTimeout = 5 * time.Second

// Frame is a single AGW frame read from the gateway.
type Frame struct {
	Hdr  *agw.Header
	Data []byte
}

// IsSessionFrame reports whether kind belongs to the per-session connected-
// mode protocol (data or disconnect). Other kinds — e.g. 'X' register acks,
// 'R'/'G'/'g' query replies — are dispatcher-level control frames that echo
// our own callsign in CallFrom and must not be routed through session
// lookup, or every reply to our own control frames looks like traffic for
// an unknown session and triggers a spurious disconnect back to tncd.
func IsSessionFrame(kind byte) bool {
	return kind == KindConnectedData || kind == KindDisconnect
}

// KeepaliveHeader builds a version-request frame: a lightweight,
// side-effect-free query that the AGWPE server always answers, used to
// reset the server's idle timer during long gaps between traffic.
func KeepaliveHeader(port uint8, callsign string) *agw.Header {
	return &agw.Header{
		Port:     port,
		DataKind: agw.KindVersion,
		CallFrom: callsign,
	}
}

// Conn is a connection to an AGWPE gateway with our callsign registered. It
// reads frames in the background, serializes writes, and sends periodic
// keepalive frames.
type Conn struct {
	cfg  Config
	conn net.Conn

	writeMu sync.Mutex

	frames chan Frame
	done   chan struct{}
	err    error

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
}

// Dial connects to the gateway named in cfg, registers cfg.Callsign, and
// starts the background reader and keepalive. ctx bounds only the dial
// itself; once Dial returns, the connection lives until Close is called or
// the gateway drops it.
func Dial(ctx context.Context, cfg Config) (*Conn, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "tcp", cfg.HostPort)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", cfg.HostPort, err)
	}

	if err := register(ctx, nc, cfg); err != nil {
		nc.Close()
		return nil, err
	}

	c := &Conn{
		cfg:    cfg,
		conn:   nc,
		frames: make(chan Frame),
		done:   make(chan struct{}),
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())

	c.wg.Add(1)
	go c.readLoop()

	if cfg.KeepAlive > 0 {
		c.wg.Add(1)
		go c.keepaliveLoop()
	}

	return c, nil
}

// register sends the 'X' frame and waits for the gateway's reply, which the
// AGWPE API document defines as a single data byte: 0x01 for success, 0x00
// for failure. It runs before the reader goroutine starts, so it reads the
// connection directly.
func register(ctx context.Context, nc net.Conn, cfg Config) error {
	regHeader := &agw.Header{
		Port:     cfg.Port(),
		DataKind: agw.KindRegisterCallsign,
		CallFrom: cfg.Callsign,
	}
	if err := agw.WriteFrame(nc, regHeader, nil); err != nil {
		return fmt.Errorf("register callsign: %w", err)
	}

	_ = nc.SetReadDeadline(time.Now().Add(registerTimeout))
	defer nc.SetReadDeadline(time.Time{})
	// Abort the read promptly if the caller gives up.
	stop := context.AfterFunc(ctx, func() { _ = nc.SetReadDeadline(time.Now()) })
	defer stop()

	for {
		hdr, data, err := agw.ReadFrame(nc)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("waiting for reply to callsign registration: %w", err)
		}
		if hdr.DataKind != agw.KindRegisterCallsign {
			continue
		}
		if len(data) > 0 && data[0] == 0 {
			return fmt.Errorf("gateway refused to register callsign %s", cfg.Callsign)
		}
		return nil
	}
}

func (c *Conn) readLoop() {
	defer c.wg.Done()
	for {
		hdr, data, err := agw.ReadFrame(c.conn)
		if err != nil {
			c.err = err
			close(c.done)
			return
		}
		select {
		case c.frames <- Frame{hdr, data}:
		case <-c.ctx.Done():
			c.err = net.ErrClosed
			close(c.done)
			return
		}
	}
}

// Periodic keepalive frame so the server's idle timer doesn't fire during
// long gaps between traffic. Any bytes read from us reset its idle timer.
func (c *Conn) keepaliveLoop() {
	defer c.wg.Done()
	ticker := time.NewTicker(c.cfg.KeepAlive)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.Write(KeepaliveHeader(c.cfg.Port(), c.cfg.Callsign), nil); err != nil {
				log.Printf("Keepalive write failed: %v", err)
			}
		}
	}
}

// Frames delivers every frame read from the gateway. It is never closed;
// watch Done to learn that the connection has failed.
func (c *Conn) Frames() <-chan Frame {
	return c.frames
}

// Done is closed once the reader stops, either because the connection failed
// or because the Conn was closed. Frames delivered before that point have
// already been received by the time Done is observed.
func (c *Conn) Done() <-chan struct{} {
	return c.done
}

// Err returns the reason the reader stopped. It is only valid after Done is
// closed.
func (c *Conn) Err() error {
	return c.err
}

// Write sends a frame to the gateway. It is safe for concurrent use.
func (c *Conn) Write(hdr *agw.Header, data []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return agw.WriteFrame(c.conn, hdr, data)
}

// Close shuts the connection down and waits for the background goroutines
// to exit. Writes in flight fail with a network error. It is safe to call
// more than once.
func (c *Conn) Close() error {
	var err error
	c.once.Do(func() {
		c.cancel()
		err = c.conn.Close()
		c.wg.Wait()
	})
	return err
}
