package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
	"unsafe"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"
	"golang.org/x/sys/unix"

	"github.com/larsks/agwtools/internal/agwconn"
	"github.com/larsks/agwtools/internal/version"
)

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
	agwconn.Config
	usePty      bool
	once        bool
	maxCons     int
	idleTimeout time.Duration
	eol         bool
}

var options Options

type writeFunc func(*agw.Header, []byte) error

func init() {
	options.AddFlags(flag.CommandLine)
	flag.BoolVarP(&options.usePty, "pty", "t", false, "allocate a pty for the command")
	flag.BoolVarP(&options.once, "once", "o", false, "exit after first command completes")
	flag.IntVarP(&options.maxCons, "max-connections", "m", 0, "maximum simultaneous connections (0 = unlimited)")
	flag.DurationVarP(&options.idleTimeout, "idle-timeout", "i", 10*time.Minute, "disconnect a session after this period of inactivity from the remote station (0 to disable)")
	flag.BoolVarP(&options.eol, "eol", "l", false, "translate \\r\\n to \\r in command output sent to the remote station")
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: agwlisten [-f <config>] [-h <agwpe_host:port>] [--pty|-t] [-c <callsign>] [-p <port>] [-m <limit>] [-k <interval>] [-i <timeout>] [--eol|-l] [--once|-o] [--version] -- <command> [<args>...]\n")
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	if options.ShowVersion {
		fmt.Println(version.VersionString("agwlisten"))
		return
	}

	configPath, err := agwconn.LoadConfigFile(flag.CommandLine, "agwlisten")
	if err != nil {
		log.Fatal(err)
	}
	if configPath != "" {
		log.Printf("Using configuration file %s", configPath)
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	if err := options.Validate(); err != nil {
		log.Fatal(err)
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
		conn, err := agwconn.Dial(ctx, options.Config)
		if err != nil {
			log.Printf("Failed to connect to AGWPE: %v", err)
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

		log.Printf("Connected to AGWPE at %s", options.HostPort)
		log.Printf("Registered callsign %s on port %d", options.Callsign, options.RadioPort)

		writeAGW := conn.Write
		frames := conn.Frames()
		readerDone := conn.Done()

		// Connection-scoped context so we can terminate sessions on drop
		connCtx, connCancel := context.WithCancel(ctx)

		activeSessions := make(map[string]chan agwconn.Frame)
		sessionDone := make(chan string)
		shuttingDown := false

		log.Printf("Listening for inbound connections...")

	dispatcherLoop:
		for {
			select {
			case <-readerDone:
				readerDone = nil // Done stays closed; don't spin on it
				log.Printf("AGWPE connection read error: %v", conn.Err())
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

				remoteCall := f.Hdr.CallFrom

				if f.Hdr.DataKind == agwconn.KindConnect {
					if _, exists := activeSessions[remoteCall]; exists {
						log.Printf("Ignoring duplicate connect frame from %s", remoteCall)
						continue
					}
					if options.maxCons > 0 && len(activeSessions) >= options.maxCons {
						log.Printf("Rejecting connection from %s (limit %d reached)", remoteCall, options.maxCons)
						rejectHdr := &agw.Header{
							Port:     options.Port(),
							DataKind: agwconn.KindDisconnect,
							CallFrom: options.Callsign,
							CallTo:   remoteCall,
						}
						writeAGW(rejectHdr, nil)
						continue
					}

					log.Printf("Inbound connection from %s", remoteCall)
					ch := make(chan agwconn.Frame, 32)
					activeSessions[remoteCall] = ch

					cfg := SessionConfig{
						RemoteCall:  remoteCall,
						Callsign:    options.Callsign,
						Port:        options.RadioPort,
						CmdName:     cmdName,
						CmdArgs:     cmdArgs,
						UsePty:      options.usePty,
						IdleTimeout: options.idleTimeout,
						CRLFToCR:    options.eol,
					}
					go handleSession(connCtx, writeAGW, cfg, ch, sessionDone)
				} else if agwconn.IsSessionFrame(f.Hdr.DataKind) {
					// Route frames to the appropriate session if it exists
					if ch, exists := activeSessions[remoteCall]; exists {
						ch <- f
					} else if f.Hdr.DataKind != agwconn.KindDisconnect {
						// We received data for an unknown session, actively drop them.
						// Disconnect frames are ignored if we don't know the session.
						log.Printf("Received frame for unknown session %s, dropping", remoteCall)
						dropHdr := &agw.Header{
							Port:     options.Port(),
							DataKind: agwconn.KindDisconnect,
							CallFrom: options.Callsign,
							CallTo:   remoteCall,
						}
						writeAGW(dropHdr, nil)
					}
				}
			}
			// Non-session frames (register acks, version/port-info/port-caps
			// replies, etc.) are control-plane responses to our own requests
			// and require no dispatcher action.
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

	log.Printf("Exiting agwlisten.")
}

func handleSession(ctx context.Context, writeAGW writeFunc, cfg SessionConfig, frames <-chan agwconn.Frame, done chan<- string) {
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
			DataKind: agwconn.KindDisconnect,
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
		buf := make([]byte, agwconn.MaxDataLen)
		var eol crlfToCR
		for {
			n, err := cmdStdout.Read(buf)
			if cfg.CRLFToCR {
				n = eol.filter(buf[:n])
			}
			if n > 0 {
				outHdr := &agw.Header{
					Port:     uint8(cfg.Port),
					DataKind: agwconn.KindConnectedData,
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
			if f.Hdr.DataKind == agwconn.KindConnectedData {
				if len(f.Data) > 0 {
					if idleTimer != nil {
						if !idleTimer.Stop() {
							<-idleTimer.C
						}
						idleTimer.Reset(cfg.IdleTimeout)
					}
					select {
					case stdinCh <- f.Data:
					case <-ctx.Done():
					}
				}
			} else if f.Hdr.DataKind == agwconn.KindDisconnect {
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
			DataKind: agwconn.KindDisconnect,
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
