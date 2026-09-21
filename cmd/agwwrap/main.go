package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"
	"golang.org/x/sys/unix"
)

const (
	KindConnect       byte = 'C'
	KindDisconnect    byte = 'd'
	KindConnectedData byte = 'D'
)

type agwFrame struct {
	hdr  *agw.Header
	data []byte
}

// isSessionFrame reports whether kind belongs to the per-session connected-
// mode protocol (data or disconnect). Other kinds — e.g. 'X' register acks,
// 'R'/'G'/'g' query replies — are dispatcher-level control frames that echo
// our own callsign in CallFrom and must not be routed through session
// lookup, or every reply to our own control frames looks like traffic for
// an unknown session and triggers a spurious disconnect back to tncd.
func isSessionFrame(kind byte) bool {
	return kind == KindConnectedData || kind == KindDisconnect
}

type SessionConfig struct {
	RemoteCall  string
	Callsign    string
	Port        int
	CmdName     string
	CmdArgs     []string
	UsePty      bool
	IdleTimeout time.Duration
	CRLFToCR    bool
}

type Options struct {
	hostPort    string
	usePty      bool
	callsign    string
	radioPort   int
	once        bool
	maxCons     int
	keepAlive   time.Duration
	idleTimeout time.Duration
	eol         bool
}

var options Options

type writeFunc func(*agw.Header, []byte) error

func init() {
	flag.StringVarP(&options.hostPort, "host", "h", "localhost:8000", "agwpe_host:port")
	flag.BoolVarP(&options.usePty, "pty", "t", false, "allocate a pty for the command")
	flag.StringVarP(&options.callsign, "callsign", "c", "NOCALL", "local callsign to register")
	flag.IntVarP(&options.radioPort, "port", "p", 0, "radio port")
	flag.BoolVarP(&options.once, "once", "o", false, "exit after first command completes")
	flag.IntVarP(&options.maxCons, "max-connections", "m", 0, "maximum simultaneous connections (0 = unlimited)")
	flag.DurationVarP(&options.keepAlive, "keepalive", "k", 60*time.Second, "interval for AGWPE keepalive frames, to prevent the server from closing an idle connection (0 to disable)")
	flag.DurationVarP(&options.idleTimeout, "idle-timeout", "i", 10*time.Minute, "disconnect a session after this period of inactivity from the remote station (0 to disable)")
	flag.BoolVarP(&options.eol, "eol", "l", false, "translate \\r\\n to \\r in command output sent to the remote station")
}

func main() {
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		log.Fatalf("Usage: agwwrap [-h <agwpe_host:port>] [--pty|-t] [-c <callsign>] [-p <port>] [-m <limit>] [-k <interval>] [-i <timeout>] [--eol|-l] [--once|-o] -- <command> [<args>...]")
	}
	cmdName := args[0]
	cmdArgs := args[1:]

	// Root context handles graceful shutdown via signals
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

connectionLoop:
	for ctx.Err() == nil {
		conn, err := net.Dial("tcp", options.hostPort)
		if err != nil {
			log.Printf("Failed to connect to %s: %v", options.hostPort, err)
			if options.once {
				break connectionLoop
			}
			select {
			case <-ctx.Done():
				break connectionLoop
			case <-time.After(5 * time.Second):
			}
			continue
		}

		log.Printf("Connected to AGWPE at %s", options.hostPort)

		// Register callsign
		regHeader := &agw.Header{
			Port:     uint8(options.radioPort),
			DataKind: agw.KindRegisterCallsign,
			CallFrom: options.callsign,
		}
		if err := agw.WriteFrame(conn, regHeader, nil); err != nil {
			log.Printf("Failed to register callsign: %v", err)
			conn.Close()
			if options.once {
				break connectionLoop
			}
			select {
			case <-ctx.Done():
				break connectionLoop
			case <-time.After(5 * time.Second):
			}
			continue
		}
		log.Printf("Registered callsign %s on port %d", options.callsign, options.radioPort)

		// Serialize writes to the AGWPE TCP socket
		var connMu sync.Mutex
		writeAGW := func(hdr *agw.Header, data []byte) error {
			connMu.Lock()
			defer connMu.Unlock()
			return agw.WriteFrame(conn, hdr, data)
		}

		// Connection-scoped context so we can terminate sessions on drop
		connCtx, connCancel := context.WithCancel(ctx)

		frames := make(chan agwFrame)
		readerErr := make(chan error, 1)

		// Centralized reader for the AGWPE connection
		go func() {
			for {
				hdr, data, err := agw.ReadFrame(conn)
				if err != nil {
					select {
					case readerErr <- err:
					case <-connCtx.Done():
					}
					return
				}
				select {
				case frames <- agwFrame{hdr, data}:
				case <-connCtx.Done():
					return
				}
			}
		}()

		activeSessions := make(map[string]chan agwFrame)
		sessionDone := make(chan string)
		shuttingDown := false

		// Periodic keepalive frame so the server's idle timer doesn't fire
		// during long gaps between inbound connections. A version request
		// is a lightweight, side-effect-free query the server always
		// answers, and any bytes read from us reset its idle timer.
		var keepaliveCh <-chan time.Time
		var keepaliveTicker *time.Ticker
		if options.keepAlive > 0 {
			keepaliveTicker = time.NewTicker(options.keepAlive)
			keepaliveCh = keepaliveTicker.C
		}

		log.Printf("Listening for inbound connections...")

	dispatcherLoop:
		for {
			select {
			case err := <-readerErr:
				log.Printf("AGWPE connection read error: %v", err)
				_ = conn.Close() // fail fast: terminate any in-flight writers
				connCancel()     // terminate all active sessions
				shuttingDown = true
				if len(activeSessions) == 0 {
					break dispatcherLoop
				}
			case <-ctx.Done():
				if !shuttingDown {
					log.Printf("Shutting down dispatcher...")
					connCancel() // terminate all active sessions
					shuttingDown = true
					if len(activeSessions) == 0 {
						break dispatcherLoop
					}
				}
			case remoteCall := <-sessionDone:
				delete(activeSessions, remoteCall)
				if shuttingDown && len(activeSessions) == 0 {
					break dispatcherLoop // all sessions finished cleanly
				}
				if options.once && !shuttingDown {
					connCancel() // trigger shutdown after first session finishes
					shuttingDown = true
					if len(activeSessions) == 0 {
						break dispatcherLoop
					}
				}
			case f := <-frames:
				if shuttingDown {
					continue
				}

				remoteCall := f.hdr.CallFrom

				if f.hdr.DataKind == KindConnect {
					if _, exists := activeSessions[remoteCall]; exists {
						log.Printf("Ignoring duplicate connect frame from %s", remoteCall)
						continue
					}
					if options.maxCons > 0 && len(activeSessions) >= options.maxCons {
						log.Printf("Rejecting connection from %s (limit %d reached)", remoteCall, options.maxCons)
						rejectHdr := &agw.Header{
							Port:     uint8(options.radioPort),
							DataKind: KindDisconnect,
							CallFrom: options.callsign,
							CallTo:   remoteCall,
						}
						writeAGW(rejectHdr, nil)
						continue
					}

					log.Printf("Inbound connection from %s", remoteCall)
					ch := make(chan agwFrame, 32)
					activeSessions[remoteCall] = ch

					cfg := SessionConfig{
						RemoteCall:  remoteCall,
						Callsign:    options.callsign,
						Port:        options.radioPort,
						CmdName:     cmdName,
						CmdArgs:     cmdArgs,
						UsePty:      options.usePty,
						IdleTimeout: options.idleTimeout,
						CRLFToCR:    options.eol,
					}
					go handleSession(connCtx, writeAGW, cfg, ch, sessionDone)
				} else if isSessionFrame(f.hdr.DataKind) {
					// Route frames to the appropriate session if it exists
					if ch, exists := activeSessions[remoteCall]; exists {
						ch <- f
					} else if f.hdr.DataKind != KindDisconnect {
						// We received data for an unknown session, actively drop them.
						// Disconnect frames are ignored if we don't know the session.
						log.Printf("Received frame for unknown session %s, dropping", remoteCall)
						dropHdr := &agw.Header{
							Port:     uint8(options.radioPort),
							DataKind: KindDisconnect,
							CallFrom: options.callsign,
							CallTo:   remoteCall,
						}
						writeAGW(dropHdr, nil)
					}
				}
			case <-keepaliveCh:
				if err := writeAGW(keepaliveHeader(uint8(options.radioPort), options.callsign), nil); err != nil {
					log.Printf("Keepalive write failed: %v", err)
				}
			}
			// Non-session frames (register acks, version/port-info/port-caps
			// replies, etc.) are control-plane responses to our own requests
			// and require no dispatcher action.
		}

		if keepaliveTicker != nil {
			keepaliveTicker.Stop()
		}
		connCancel()
		conn.Close()

		if options.once || ctx.Err() != nil {
			break connectionLoop
		}

		log.Printf("Reconnecting in 5 seconds...")
		select {
		case <-ctx.Done():
			break connectionLoop
		case <-time.After(5 * time.Second):
		}
	}

	log.Printf("Exiting agwwrap.")
}

// keepaliveHeader builds a version-request frame: a lightweight,
// side-effect-free query that the AGWPE server always answers, used to
// reset the server's idle timer during long gaps between inbound
// connections.
func keepaliveHeader(port uint8, callsign string) *agw.Header {
	return &agw.Header{
		Port:     port,
		DataKind: agw.KindVersion,
		CallFrom: callsign,
	}
}

func handleSession(ctx context.Context, writeAGW writeFunc, cfg SessionConfig, frames <-chan agwFrame, done chan<- string) {
	// Always notify the dispatcher when this session ends
	defer func() { done <- cfg.RemoteCall }()

	cmdCtx, cmdCancel := context.WithCancel(ctx)
	defer cmdCancel()

	cmd := exec.CommandContext(cmdCtx, cfg.CmdName, cfg.CmdArgs...)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("SRC_CALLSIGN=%s", cfg.RemoteCall),
		fmt.Sprintf("DST_CALLSIGN=%s", cfg.Callsign),
	)

	var cmdStdin io.WriteCloser
	var cmdStdout io.ReadCloser
	var master, slave *os.File
	var err error

	if cfg.UsePty {
		master, slave, err = openPTY()
		if err != nil {
			log.Printf("[%s] Failed to open PTY: %v", cfg.RemoteCall, err)
			return
		}
		cmd.Stdin = slave
		cmd.Stdout = slave
		cmd.Stderr = slave
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0,
		}
		cmdStdin = master
		cmdStdout = master
	} else {
		cmdStdin, err = cmd.StdinPipe()
		if err != nil {
			log.Printf("[%s] Failed to get stdin pipe: %v", cfg.RemoteCall, err)
			return
		}
		cmdStdout, err = cmd.StdoutPipe()
		if err != nil {
			log.Printf("[%s] Failed to get stdout pipe: %v", cfg.RemoteCall, err)
			return
		}
		cmd.Stderr = os.Stderr
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[%s] Failed to start command: %v", cfg.RemoteCall, err)
		if cfg.UsePty {
			master.Close()
			slave.Close()
		}
		discHdr := &agw.Header{
			Port:     uint8(cfg.Port),
			DataKind: KindDisconnect,
			CallFrom: cfg.Callsign,
			CallTo:   cfg.RemoteCall,
		}
		writeAGW(discHdr, nil)
		return
	}

	if cfg.UsePty {
		if err := slave.Close(); err != nil {
			log.Printf("[%s] Failed to close PTY slave in parent: %v", cfg.RemoteCall, err)
		}
	}

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	stdinCh := make(chan []byte, 32)
	stdinWriteErr := make(chan error, 1)
	go func() {
		for data := range stdinCh {
			if _, err := cmdStdin.Write(data); err != nil {
				stdinWriteErr <- err
				for range stdinCh {
				}
				return
			}
		}
	}()

	writerErr := make(chan error, 1)
	// Writer: Command stdout -> AGWPE
	go func() {
		buf := make([]byte, 256)
		var eol crlfToCR
		for {
			n, err := cmdStdout.Read(buf)
			if cfg.CRLFToCR {
				n = eol.filter(buf[:n])
			}
			if n > 0 {
				outHdr := &agw.Header{
					Port:     uint8(cfg.Port),
					DataKind: KindConnectedData,
					CallFrom: cfg.Callsign,
					CallTo:   cfg.RemoteCall,
				}
				if writeErr := writeAGW(outHdr, buf[:n]); writeErr != nil {
					writerErr <- fmt.Errorf("AGWPE write error: %w", writeErr)
					return
				}
			}
			if err != nil {
				if isCommandStreamClosed(err, cfg.UsePty) {
					writerErr <- nil
				} else {
					writerErr <- fmt.Errorf("Command read error: %w", err)
				}
				return
			}
		}
	}()

	disconnectedByRemote := false

	// Idle timer: disconnect a session that receives nothing from the
	// remote station for cfg.IdleTimeout, so a dropped or forgotten
	// connection doesn't keep its command running indefinitely.
	var idleTimer *time.Timer
	var idleCh <-chan time.Time
	if cfg.IdleTimeout > 0 {
		idleTimer = time.NewTimer(cfg.IdleTimeout)
		idleCh = idleTimer.C
		defer idleTimer.Stop()
	}

runLoop:
	for {
		select {
		case <-ctx.Done():
			cmdCancel()
			<-cmdDone
			break runLoop
		case err := <-writerErr:
			if err != nil {
				log.Printf("[%s] Write error: %v", cfg.RemoteCall, err)
			}
			cmdCancel()
			<-cmdDone
			break runLoop
		case err := <-stdinWriteErr:
			log.Printf("[%s] Command stdin write error: %v", cfg.RemoteCall, err)
			cmdCancel()
			<-cmdDone
			break runLoop
		case <-idleCh:
			log.Printf("[%s] Idle timeout (%s) reached, disconnecting", cfg.RemoteCall, cfg.IdleTimeout)
			cmdCancel()
			<-cmdDone
			break runLoop
		case f := <-frames:
			if f.hdr.DataKind == KindConnectedData {
				if len(f.data) > 0 {
					if idleTimer != nil {
						if !idleTimer.Stop() {
							<-idleTimer.C
						}
						idleTimer.Reset(cfg.IdleTimeout)
					}
					select {
					case stdinCh <- f.data:
					case <-ctx.Done():
					}
				}
			} else if f.hdr.DataKind == KindDisconnect {
				log.Printf("[%s] Disconnected by remote", cfg.RemoteCall)
				disconnectedByRemote = true
				cmdCancel()
				<-cmdDone
				break runLoop
			}
		case err := <-cmdDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[%s] Command exited with error: %v", cfg.RemoteCall, err)
			} else {
				log.Printf("[%s] Command finished.", cfg.RemoteCall)
			}
			break runLoop
		}
	}

	close(stdinCh)
	if cfg.UsePty {
		master.Close()
	} else {
		cmdStdin.Close()
	}

	log.Printf("[%s] Cleaning up session", cfg.RemoteCall)

	// Send disconnect frame if we initiated the closure
	if !disconnectedByRemote {
		discHdr := &agw.Header{
			Port:     uint8(cfg.Port),
			DataKind: KindDisconnect,
			CallFrom: cfg.Callsign,
			CallTo:   cfg.RemoteCall,
		}
		writeAGW(discHdr, nil)
	}
}

// crlfToCR translates \r\n to \r in a byte stream that arrives in
// arbitrary chunks. A \r is passed through as soon as it is seen (so a
// prompt ending in a lone \r isn't held back waiting for more data);
// prevCR records that the last byte emitted was a \r so a \n that starts the
// next chunk can still be dropped. A bare \n is left alone.
type crlfToCR struct {
	prevCR bool
}

// filter rewrites buf in place and returns the number of bytes kept. The
// output is never longer than the input, so no allocation is needed.
func (f *crlfToCR) filter(buf []byte) int {
	n := 0
	for _, b := range buf {
		if b == '\n' && f.prevCR {
			f.prevCR = false
			continue
		}
		f.prevCR = b == '\r'
		buf[n] = b
		n++
	}
	return n
}

// isCommandStreamClosed reports whether err from reading a command's output
// stream indicates a normal end-of-stream. Pipes signal this with io.EOF.
// On Linux, once the last open fd on a PTY slave is closed, reads on the
// master return EIO instead of EOF, so that case is treated as benign too
// when usePty is set.
func isCommandStreamClosed(err error, usePty bool) bool {
	return err == io.EOF || (usePty && errors.Is(err, syscall.EIO))
}

func openPTY() (master, slave *os.File, err error) {
	mfd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}

	lockVal := 0
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(mfd), unix.TIOCSPTLCK, uintptr(unsafe.Pointer(&lockVal))); errno != 0 {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("unlockpt: %w", errno)
	}

	ptsNum, err := unix.IoctlGetUint32(mfd, unix.TIOCGPTN)
	if err != nil {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("ptsname: %w", err)
	}
	slaveName := fmt.Sprintf("/dev/pts/%d", ptsNum)

	if err := unix.SetNonblock(mfd, true); err != nil {
		unix.Close(mfd)
		return nil, nil, fmt.Errorf("set nonblock on master: %w", err)
	}
	masterFile := os.NewFile(uintptr(mfd), slaveName)

	sfd, err := unix.Open(slaveName, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		masterFile.Close()
		return nil, nil, fmt.Errorf("open slave %s: %w", slaveName, err)
	}
	slaveFile := os.NewFile(uintptr(sfd), slaveName)

	return masterFile, slaveFile, nil
}
