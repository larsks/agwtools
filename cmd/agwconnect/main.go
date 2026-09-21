package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/chrissnell/graywolf/pkg/agw"
	flag "github.com/spf13/pflag"
	"golang.org/x/sys/unix"

	"github.com/larsks/agwtools/internal/agwconn"
	"github.com/larsks/agwtools/internal/version"
)

const (
	// pidNoLayer3 is the PID for plain connected-mode text.
	pidNoLayer3 = 0xF0

	// maxCallsignLen is the longest callsign (including SSID) that fits in
	// the 10-byte AGW header field along with its NUL terminator.
	maxCallsignLen = 9

	// maxVia is the AX.25 limit on digipeaters in a path. The AGWPE API
	// document says "max 7" for 'v' frames, but graywolf accepts eight, so
	// the AX.25 limit is used here.
	maxVia = 8

	// defaultWait is how many seconds to keep listening after end of input
	// when stdin is not a terminal and --wait was not given.
	defaultWait = 30
)

// disconnectWait bounds how long we wait for the remote station to
// acknowledge a disconnect before giving up and exiting anyway. It is a
// variable so tests can shorten it.
var disconnectWait = 5 * time.Second

type Options struct {
	agwconn.Config
	via  []string
	raw  bool
	wait int // seconds, as given by --wait

	// eofTimeout is how long run() keeps listening for the remote station
	// after end of input, resolved from wait by prepare. Zero means
	// disconnect at once.
	eofTimeout time.Duration
}

var options Options

func init() {
	options.AddFlags(flag.CommandLine)
	flag.StringSliceVarP(&options.via, "via", "v", nil, "digipeater path, comma-separated or repeated (e.g. -v WIDE1-1,WIDE2-1)")
	flag.BoolVarP(&options.raw, "raw", "r", false, "do not translate line endings (default: \\n <-> \\r)")
	flag.IntVarP(&options.wait, "wait", "w", 0, "after end of input, keep the session open until the remote station has been silent for this many seconds, then disconnect; 0 disconnects at once (default: 30 if stdin is not a terminal, otherwise 0)")
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage: agwconnect [-f <config>] [-h <agwpe_host:port>] [-c <callsign>] [-p <port>] [-k <interval>] [-v <digipeaters>] [--raw|-r] [-w <seconds>] [--version] <remote-callsign>\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("agwconnect: ")

	flag.Usage = usage
	flag.Parse()

	if options.ShowVersion {
		fmt.Println(version.VersionString("agwconnect"))
		return
	}

	if _, err := agwconn.LoadConfigFile(flag.CommandLine, "agwconnect"); err != nil {
		fmt.Fprintf(os.Stderr, "agwconnect: %v\n", err)
		os.Exit(2)
	}

	remote, err := options.prepare(flag.Args(), flag.CommandLine.Changed("wait"), isTerminal(os.Stdin))
	if err != nil {
		fmt.Fprintf(os.Stderr, "agwconnect: %v\n", err)
		usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// Once the first signal has begun a graceful disconnect, let a
		// second one kill the process in the usual way.
		<-ctx.Done()
		stop()
	}()

	err = run(ctx, options, remote, os.Stdin, os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		os.Exit(130)
	default:
		log.Print(err)
		os.Exit(1)
	}
}

// isTerminal reports whether f is connected to a terminal.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// resolveWait turns the --wait option into the time to keep listening after
// end of input. Unless --wait was given, a person at a terminal who types
// Ctrl-D wants out, so the answer is zero; piped input is typically a script
// whose replies are still on their way, so it defaults to defaultWait.
func resolveWait(seconds int, set, stdinIsTerminal bool) time.Duration {
	if !set {
		if stdinIsTerminal {
			return 0
		}
		seconds = defaultWait
	}
	return time.Duration(seconds) * time.Second
}

// prepare validates the options and command line arguments, returning the
// remote callsign. Digipeater callsigns are normalized to upper case. waitSet
// says whether --wait was given explicitly, and stdinIsTerminal whether
// stdin is a terminal; together they decide o.eofTimeout.
func (o *Options) prepare(args []string, waitSet, stdinIsTerminal bool) (string, error) {
	if len(args) != 1 {
		return "", errors.New("expected exactly one remote callsign")
	}
	if err := o.Validate(); err != nil {
		return "", err
	}
	if o.wait < 0 {
		return "", fmt.Errorf("wait %d must not be negative", o.wait)
	}
	o.eofTimeout = resolveWait(o.wait, waitSet, stdinIsTerminal)

	remote := strings.ToUpper(args[0])
	if err := checkCallsign(remote); err != nil {
		return "", err
	}

	if len(o.via) > maxVia {
		return "", fmt.Errorf("too many digipeaters (%d, maximum %d)", len(o.via), maxVia)
	}
	for i, v := range o.via {
		v = strings.ToUpper(strings.TrimSpace(v))
		if err := checkCallsign(v); err != nil {
			return "", fmt.Errorf("digipeater: %w", err)
		}
		o.via[i] = v
	}

	return remote, nil
}

func checkCallsign(call string) error {
	switch {
	case call == "":
		return errors.New("empty callsign")
	case len(call) > maxCallsignLen:
		return fmt.Errorf("callsign %q is longer than %d characters", call, maxCallsignLen)
	case strings.ContainsAny(call, " \t\r\n\x00,"):
		return fmt.Errorf("callsign %q contains invalid characters", call)
	}
	return nil
}

// viaPayload encodes a digipeater path for a 'v' frame: one byte giving the
// number of digipeaters, then each callsign as 10 NUL-padded bytes.
func viaPayload(via []string) []byte {
	const callLen = 10
	buf := make([]byte, 1+len(via)*callLen)
	buf[0] = byte(len(via))
	for i, v := range via {
		copy(buf[1+i*callLen:1+(i+1)*callLen], v)
	}
	return buf
}

// eolFilter rewrites any of \r, \n or \r\n to the single byte out. It
// works on a stream that arrives in arbitrary chunks: prevCR records that the
// last byte seen was a \r, so a \n that begins the next chunk is still
// recognized as the second half of a \r\n pair.
//
// Packet hosts end lines with \r, while local terminals and pipes use \n, so
// one filter with out='\r' handles the outbound direction and another with
// out='\n' handles the inbound one.
type eolFilter struct {
	out    byte
	prevCR bool
}

// filter rewrites buf in place and returns the number of bytes kept. The
// output is never longer than the input, so no allocation is needed.
func (f *eolFilter) filter(buf []byte) int {
	n := 0
	for _, b := range buf {
		switch b {
		case '\r':
			f.prevCR = true
			buf[n] = f.out
			n++
		case '\n':
			if f.prevCR {
				f.prevCR = false
				continue
			}
			buf[n] = f.out
			n++
		default:
			f.prevCR = false
			buf[n] = b
			n++
		}
	}
	return n
}

type session struct {
	conn   *agwconn.Conn
	opts   Options
	remote string
	stdout io.Writer
	stderr io.Writer

	toRemote   eolFilter // used only by pumpStdin
	fromRemote eolFilter // used only by the bridge loop
}

// run connects to the gateway, raises a link to remote, and shuttles bytes
// between the link and stdin/stdout until either side finishes.
func run(ctx context.Context, opts Options, remote string, stdin io.Reader, stdout, stderr io.Writer) error {
	conn, err := agwconn.Dial(ctx, opts.Config)
	if err != nil {
		return err
	}
	defer conn.Close()

	s := &session{
		conn:       conn,
		opts:       opts,
		remote:     remote,
		stdout:     stdout,
		stderr:     stderr,
		toRemote:   eolFilter{out: '\r'},
		fromRemote: eolFilter{out: '\n'},
	}

	path := ""
	if len(opts.via) > 0 {
		path = " via " + strings.Join(opts.via, ",")
	}
	fmt.Fprintf(stderr, "Connecting to %s%s...\n", remote, path)

	if err := s.connect(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Connected to %s\n", remote)

	return s.bridge(ctx, stdin)
}

func (s *session) header(kind byte) *agw.Header {
	return &agw.Header{
		Port:     s.opts.Port(),
		DataKind: kind,
		CallFrom: s.opts.Callsign,
		CallTo:   s.remote,
	}
}

// isFromRemote reports whether f is a session or connect frame from the
// station we are talking to.
func (s *session) isFromRemote(f agwconn.Frame) bool {
	if !strings.EqualFold(f.Hdr.CallFrom, s.remote) {
		return false
	}
	return f.Hdr.DataKind == agwconn.KindConnect || agwconn.IsSessionFrame(f.Hdr.DataKind)
}

// statusText tidies the text the gateway attaches to 'C' and 'd' frames.
func statusText(data []byte) string {
	return strings.TrimRight(string(data), "\r\n\x00 ")
}

// connect requests the link and waits for the gateway to report the outcome:
// a 'C' frame means connected, a 'd' frame means the attempt failed.
func (s *session) connect(ctx context.Context) error {
	hdr := s.header(agwconn.KindConnect)
	var data []byte
	if len(s.opts.via) > 0 {
		hdr.DataKind = agwconn.KindConnectVia
		data = viaPayload(s.opts.via)
	}
	if err := s.conn.Write(hdr, data); err != nil {
		return fmt.Errorf("send connect request: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			// Abandon the attempt so the gateway doesn't keep trying.
			_ = s.conn.Write(s.header(agwconn.KindDisconnect), nil)
			return ctx.Err()
		case <-s.conn.Done():
			return fmt.Errorf("connection to AGWPE lost: %v", s.conn.Err())
		case f := <-s.conn.Frames():
			if !s.isFromRemote(f) {
				continue
			}
			switch f.Hdr.DataKind {
			case agwconn.KindConnect:
				return nil
			case agwconn.KindDisconnect:
				if msg := statusText(f.Data); msg != "" {
					return fmt.Errorf("connection to %s failed: %s", s.remote, msg)
				}
				return fmt.Errorf("connection to %s failed", s.remote)
			}
		}
	}
}

// pumpStdin sends everything read from stdin to the remote station. It
// reports io.EOF when stdin is exhausted, or the first error otherwise.
func (s *session) pumpStdin(stdin io.Reader, done chan<- error) {
	buf := make([]byte, agwconn.MaxDataLen)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			data := buf[:n]
			if !s.opts.raw {
				data = data[:s.toRemote.filter(data)]
			}
			if len(data) > 0 {
				hdr := s.header(agwconn.KindConnectedData)
				hdr.PID = pidNoLayer3
				if werr := s.conn.Write(hdr, data); werr != nil {
					done <- fmt.Errorf("send data: %w", werr)
					return
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				err = fmt.Errorf("read stdin: %w", err)
			}
			done <- err
			return
		}
	}
}

// deliver writes data received from the remote station to stdout.
func (s *session) deliver(data []byte) error {
	if !s.opts.raw {
		data = data[:s.fromRemote.filter(data)]
	}
	_, err := s.stdout.Write(data)
	return err
}

// bridge runs the established link until it ends. It returns nil when the
// link closes normally, whichever side closed it.
func (s *session) bridge(ctx context.Context, stdin io.Reader) error {
	inDone := make(chan error, 1)
	go s.pumpStdin(stdin, inDone)

	// Once stdin is exhausted we keep reading replies, but only until the
	// remote has been quiet for eofTimeout.
	var eofTimer *time.Timer
	var eofCh <-chan time.Time
	defer func() {
		if eofTimer != nil {
			eofTimer.Stop()
		}
	}()

	for {
		select {
		case <-ctx.Done():
			s.disconnect()
			return nil
		case <-s.conn.Done():
			return fmt.Errorf("connection to AGWPE lost: %v", s.conn.Err())
		case err := <-inDone:
			inDone = nil // the pump has exited; don't select on it again
			if err != io.EOF {
				s.disconnect()
				return err
			}
			if s.opts.eofTimeout <= 0 {
				s.disconnect()
				return nil
			}
			fmt.Fprintf(s.stderr, "End of input; waiting for %s until it has been silent for %s\n", s.remote, s.opts.eofTimeout)
			eofTimer = time.NewTimer(s.opts.eofTimeout)
			eofCh = eofTimer.C
		case <-eofCh:
			fmt.Fprintf(s.stderr, "No data from %s for %s; disconnecting\n", s.remote, s.opts.eofTimeout)
			s.disconnect()
			return nil
		case f := <-s.conn.Frames():
			if !s.isFromRemote(f) {
				continue
			}
			switch f.Hdr.DataKind {
			case agwconn.KindConnectedData:
				if err := s.deliver(f.Data); err != nil {
					s.disconnect()
					return fmt.Errorf("write stdout: %w", err)
				}
				if eofTimer != nil {
					eofTimer.Reset(s.opts.eofTimeout)
				}
			case agwconn.KindDisconnect:
				fmt.Fprintf(s.stderr, "Disconnected by %s\n", s.remote)
				return nil
			}
		}
	}
}

// disconnect asks the gateway to close the link, then waits briefly for the
// remote station to confirm, still printing any data that arrives meanwhile.
func (s *session) disconnect() {
	_ = s.conn.Write(s.header(agwconn.KindDisconnect), nil)

	timer := time.NewTimer(disconnectWait)
	defer timer.Stop()
	for {
		select {
		case f := <-s.conn.Frames():
			if !s.isFromRemote(f) {
				continue
			}
			switch f.Hdr.DataKind {
			case agwconn.KindConnectedData:
				_ = s.deliver(f.Data)
			case agwconn.KindDisconnect:
				fmt.Fprintf(s.stderr, "Disconnected from %s\n", s.remote)
				return
			}
		case <-s.conn.Done():
			return
		case <-timer.C:
			fmt.Fprintf(s.stderr, "Timed out waiting for %s to confirm disconnect\n", s.remote)
			return
		}
	}
}
