// Package agwconn holds the pieces shared by the agwtools commands: the
// common command line options and a connection to an AGWPE gateway that has
// already registered our callsign.
package agwconn

import (
	"fmt"
	"time"

	flag "github.com/spf13/pflag"
)

// Config describes how to reach the AGWPE gateway and which callsign and
// radio port to use once connected.
type Config struct {
	HostPort  string
	Callsign  string
	RadioPort int
	KeepAlive time.Duration
}

// AddFlags registers the options every command shares (--host/-h,
// --callsign/-c, --port/-p and --keepalive/-k) on fs, storing the results in
// c, along with --config/-f, which names the configuration file read by
// LoadConfigFile. Call it before parsing.
func (c *Config) AddFlags(fs *flag.FlagSet) {
	fs.StringVarP(&c.HostPort, "host", "h", "localhost:8000", "agwpe_host:port")
	fs.StringVarP(&c.Callsign, "callsign", "c", "NOCALL", "local callsign to register")
	fs.IntVarP(&c.RadioPort, "port", "p", 0, "radio port")
	fs.StringP(configFlag, "f", "", "configuration file (default: $"+ConfigPathEnv+", else $XDG_CONFIG_HOME/agwtools/config.toml, else ~/.config/agwtools/config.toml; \"\" to read none)")
	fs.DurationVarP(&c.KeepAlive, "keepalive", "k", 60*time.Second, "interval for AGWPE keepalive frames, to prevent the server from closing an idle connection (0 to disable)")
}

// Validate reports whether the options are usable. The radio port is a
// single byte on the wire, so anything outside 0-255 would otherwise be
// silently truncated.
func (c *Config) Validate() error {
	if c.RadioPort < 0 || c.RadioPort > 255 {
		return fmt.Errorf("radio port %d out of range (0-255)", c.RadioPort)
	}
	if c.KeepAlive < 0 {
		return fmt.Errorf("keepalive interval %s must not be negative", c.KeepAlive)
	}
	return nil
}

// Port returns the radio port as it appears in an AGW header.
func (c *Config) Port() uint8 {
	return uint8(c.RadioPort)
}
