package agwconn

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	flag "github.com/spf13/pflag"
)

// listenFlags and connectFlags mimic the option sets of the two commands: the
// shared options plus a few of the command's own.
type testFlags struct {
	fs   *flag.FlagSet
	cfg  Config
	pty  bool
	max  int
	idle time.Duration
	via  []string
	raw  bool
	wait int
}

func listenFlags() *testFlags {
	f := &testFlags{fs: flag.NewFlagSet("agwlisten", flag.ContinueOnError)}
	f.cfg.AddFlags(f.fs)
	f.fs.BoolVarP(&f.pty, "pty", "t", false, "")
	f.fs.IntVarP(&f.max, "max-connections", "m", 0, "")
	f.fs.DurationVarP(&f.idle, "idle-timeout", "i", 10*time.Minute, "")
	return f
}

func connectFlags() *testFlags {
	f := &testFlags{fs: flag.NewFlagSet("agwconnect", flag.ContinueOnError)}
	f.cfg.AddFlags(f.fs)
	f.fs.StringSliceVarP(&f.via, "via", "v", nil, "")
	f.fs.BoolVarP(&f.raw, "raw", "r", false, "")
	f.fs.IntVarP(&f.wait, "wait", "w", 0, "")
	return f
}

func (f *testFlags) parse(t *testing.T, args ...string) {
	t.Helper()
	if err := f.fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func noHome() (string, error) { return "", errors.New("no home") }

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// load parses args, then applies the config file at path (given with
// --config) for command.
func (f *testFlags) load(t *testing.T, command, path string, args ...string) (string, error) {
	t.Helper()
	f.parse(t, append([]string{"--config", path}, args...)...)
	return loadConfigFile(f.fs, command, env(nil), noHome)
}

func TestAddFlagsRegistersConfig(t *testing.T) {
	var c Config
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	c.AddFlags(fs)

	f := fs.Lookup("config")
	if f == nil {
		t.Fatal("--config is not defined")
	}
	if f.Shorthand != "f" {
		t.Errorf("--config shorthand = %q, want f", f.Shorthand)
	}

	// sharedFlags is what the config file treats as shared; it must match
	// what AddFlags actually registers, or [shared] would drift from the
	// command line.
	var got []string
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name != "config" {
			got = append(got, f.Name)
		}
	})
	slices.Sort(got)
	if !slices.Equal(got, sharedFlags) {
		t.Errorf("AddFlags registered %v, sharedFlags is %v", got, sharedFlags)
	}
}

func TestResolveConfigPath(t *testing.T) {
	homeDir := func() (string, error) { return "/home/u", nil }

	cases := []struct {
		name         string
		args         []string
		env          map[string]string
		home         func() (string, error)
		wantPath     string
		wantExplicit bool
	}{
		{"flag", []string{"-f", "/a.toml"}, nil, homeDir, "/a.toml", true},
		{"flag beats env", []string{"--config", "/a.toml"}, map[string]string{ConfigPathEnv: "/e.toml"}, homeDir, "/a.toml", true},
		{"empty flag reads nothing", []string{"--config", ""}, map[string]string{ConfigPathEnv: "/e.toml"}, homeDir, "", true},
		{"env", nil, map[string]string{ConfigPathEnv: "/e.toml", "XDG_CONFIG_HOME": "/x"}, homeDir, "/e.toml", true},
		{"xdg", nil, map[string]string{"XDG_CONFIG_HOME": "/x"}, homeDir, "/x/agwtools/config.toml", false},
		{"empty xdg falls back to home", nil, map[string]string{"XDG_CONFIG_HOME": ""}, homeDir, "/home/u/.config/agwtools/config.toml", false},
		{"home", nil, nil, homeDir, "/home/u/.config/agwtools/config.toml", false},
		{"nothing to go on", nil, nil, noHome, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := listenFlags()
			f.parse(t, tc.args...)
			path, explicit := resolveConfigPath(f.fs, env(tc.env), tc.home)
			if path != tc.wantPath || explicit != tc.wantExplicit {
				t.Errorf("got (%q, %v), want (%q, %v)", path, explicit, tc.wantPath, tc.wantExplicit)
			}
		})
	}
}

func TestSharedSectionApplies(t *testing.T) {
	path := writeConfig(t, `
[shared]
host = "gw.example:9000"
callsign = "N0CALL-7"
port = 2
keepalive = "5s"
`)
	for name, f := range map[string]*testFlags{"agwlisten": listenFlags(), "agwconnect": connectFlags()} {
		t.Run(name, func(t *testing.T) {
			got, err := f.load(t, name, path)
			if err != nil {
				t.Fatal(err)
			}
			if got != path {
				t.Errorf("loaded %q, want %q", got, path)
			}
			want := Config{HostPort: "gw.example:9000", Callsign: "N0CALL-7", RadioPort: 2, KeepAlive: 5 * time.Second}
			if f.cfg != want {
				t.Errorf("got %+v, want %+v", f.cfg, want)
			}
		})
	}
}

func TestCommandSectionOverridesShared(t *testing.T) {
	path := writeConfig(t, `
[shared]
host = "shared:1"
callsign = "N0CALL"

[agwlisten]
host = "listen:2"

[agwconnect]
host = "connect:3"
`)
	l, c := listenFlags(), connectFlags()
	if _, err := l.load(t, "agwlisten", path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.load(t, "agwconnect", path); err != nil {
		t.Fatal(err)
	}

	if l.cfg.HostPort != "listen:2" || c.cfg.HostPort != "connect:3" {
		t.Errorf("hosts = %q and %q, want each command's own", l.cfg.HostPort, c.cfg.HostPort)
	}
	if l.cfg.Callsign != "N0CALL" || c.cfg.Callsign != "N0CALL" {
		t.Errorf("callsigns = %q and %q, want the shared one", l.cfg.Callsign, c.cfg.Callsign)
	}
}

func TestCommandLineOverridesFile(t *testing.T) {
	path := writeConfig(t, `
[shared]
host = "shared:1"
port = 4

[agwlisten]
host = "listen:2"
pty = true
`)
	f := listenFlags()
	if _, err := f.load(t, "agwlisten", path, "-h", "cli:9", "--pty=false"); err != nil {
		t.Fatal(err)
	}
	if f.cfg.HostPort != "cli:9" {
		t.Errorf("host = %q, want the command line's", f.cfg.HostPort)
	}
	if f.pty {
		t.Error("pty = true, want the command line's false")
	}
	if f.cfg.RadioPort != 4 {
		t.Errorf("port = %d, want 4 from the file, since the command line left it alone", f.cfg.RadioPort)
	}
}

func TestOptionTypes(t *testing.T) {
	l := listenFlags()
	path := writeConfig(t, `
[agwlisten]
pty = true
max-connections = 3
idle-timeout = "90s"
`)
	if _, err := l.load(t, "agwlisten", path); err != nil {
		t.Fatal(err)
	}
	if !l.pty || l.max != 3 || l.idle != 90*time.Second {
		t.Errorf("pty=%v max=%d idle=%s", l.pty, l.max, l.idle)
	}

	for name, body := range map[string]string{
		"list":   `via = ["WIDE1-1", "WIDE2-1"]`,
		"string": `via = "WIDE1-1,WIDE2-1"`,
	} {
		t.Run("via "+name, func(t *testing.T) {
			c := connectFlags()
			path := writeConfig(t, "[agwconnect]\n"+body+"\nraw = true\nwait = 12\n")
			if _, err := c.load(t, "agwconnect", path); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.via, []string{"WIDE1-1", "WIDE2-1"}) {
				t.Errorf("via = %q", c.via)
			}
			if !c.raw || c.wait != 12 {
				t.Errorf("raw=%v wait=%d", c.raw, c.wait)
			}
		})
	}
}

// TestConfigCountsAsExplicit matters to agwconnect, which treats --wait as an
// explicit choice, whether from the command line or the file, when deciding
// its default.
func TestConfigCountsAsExplicit(t *testing.T) {
	c := connectFlags()
	path := writeConfig(t, "[agwconnect]\nwait = 0\n")
	if _, err := c.load(t, "agwconnect", path); err != nil {
		t.Fatal(err)
	}
	if !c.fs.Changed("wait") {
		t.Error("wait set in the config file is not reported as Changed")
	}
}

func TestOtherCommandsSectionIsIgnored(t *testing.T) {
	// agwconnect knows nothing of pty or the typo below, and must not care:
	// both live in a section it never reads.
	path := writeConfig(t, `
[shared]
host = "shared:1"

[agwlisten]
pty = true
no-such-option = 1
`)
	c := connectFlags()
	if _, err := c.load(t, "agwconnect", path); err != nil {
		t.Fatalf("agwconnect rejected agwlisten's section: %v", err)
	}
	if c.cfg.HostPort != "shared:1" {
		t.Errorf("host = %q", c.cfg.HostPort)
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name    string
		command string
		toml    string
		want    []string // substrings the error must contain
	}{
		{
			"command option in shared", "agwlisten",
			"[shared]\npty = true\n",
			[]string{"[shared]", `"pty"`, "specific to agwlisten", "[agwlisten]"},
		},
		{
			"unknown option in shared", "agwlisten",
			"[shared]\nbogus = 1\n",
			[]string{"[shared]", `"bogus"`, "unknown"},
		},
		{
			"command option unknown to this command", "agwconnect",
			"[agwconnect]\npty = true\n",
			[]string{"[agwconnect]", `"pty"`, "unknown"},
		},
		{
			"unknown option in own section", "agwlisten",
			"[agwlisten]\nbogus = 1\n",
			[]string{"[agwlisten]", `"bogus"`, "idle-timeout"},
		},
		{
			"config key in file", "agwlisten",
			"[shared]\nconfig = \"/x\"\n",
			[]string{`"config"`},
		},
		{
			"unknown section", "agwlisten",
			"[agwlistn]\nhost = \"x\"\n",
			[]string{"[agwlistn]", "unknown section"},
		},
		{
			"option outside a section", "agwlisten",
			"host = \"x\"\n",
			[]string{`"host"`, "section"},
		},
		{
			"wrong type", "agwlisten",
			"[shared]\nport = \"abc\"\n",
			[]string{"[shared]", "port"},
		},
		{
			"duration needs a unit", "agwlisten",
			"[shared]\nkeepalive = 60\n",
			[]string{"[shared]", "keepalive"},
		},
		{
			"list of non-strings", "agwconnect",
			"[agwconnect]\nvia = [1, 2]\n",
			[]string{"via", "strings"},
		},
		{
			"unsupported type", "agwlisten",
			"[shared]\nhost = 1.5\n",
			[]string{"host", "float64"},
		},
		{
			"invalid toml", "agwlisten",
			"[shared\nhost = 1\n",
			[]string{"config file"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.toml)
			f := listenFlags()
			if tc.command == "agwconnect" {
				f = connectFlags()
			}
			_, err := f.load(t, tc.command, path)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the file", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.toml")

	t.Run("default location is silent", func(t *testing.T) {
		f := listenFlags()
		f.parse(t)
		got, err := loadConfigFile(f.fs, "agwlisten", env(map[string]string{"XDG_CONFIG_HOME": dir}), noHome)
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want no file and no error", got, err)
		}
	})

	t.Run("home fallback is silent", func(t *testing.T) {
		f := listenFlags()
		f.parse(t)
		got, err := loadConfigFile(f.fs, "agwlisten", env(nil), func() (string, error) { return dir, nil })
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want no file and no error", got, err)
		}
	})

	t.Run("no home is silent", func(t *testing.T) {
		f := listenFlags()
		f.parse(t)
		if got, err := loadConfigFile(f.fs, "agwlisten", env(nil), noHome); err != nil || got != "" {
			t.Errorf("got (%q, %v), want no file and no error", got, err)
		}
	})

	t.Run("--config is an error", func(t *testing.T) {
		f := listenFlags()
		f.parse(t, "--config", missing)
		_, err := loadConfigFile(f.fs, "agwlisten", env(nil), noHome)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), missing) {
			t.Errorf("error = %v, want a not-exist error naming %s", err, missing)
		}
	})

	t.Run("environment variable is an error", func(t *testing.T) {
		f := listenFlags()
		f.parse(t)
		_, err := loadConfigFile(f.fs, "agwlisten", env(map[string]string{ConfigPathEnv: missing}), noHome)
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want a not-exist error", err)
		}
	})

	t.Run("empty --config reads nothing", func(t *testing.T) {
		f := listenFlags()
		f.parse(t, "--config", "")
		got, err := loadConfigFile(f.fs, "agwlisten", env(map[string]string{ConfigPathEnv: missing}), noHome)
		if err != nil || got != "" {
			t.Errorf("got (%q, %v), want no file and no error", got, err)
		}
	})
}

func TestDefaultLocationIsRead(t *testing.T) {
	xdg := t.TempDir()
	path := filepath.Join(xdg, "agwtools", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[shared]\ncallsign = \"N0CALL\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f := listenFlags()
	f.parse(t)
	got, err := loadConfigFile(f.fs, "agwlisten", env(map[string]string{"XDG_CONFIG_HOME": xdg}), noHome)
	if err != nil {
		t.Fatal(err)
	}
	if got != path || f.cfg.Callsign != "N0CALL" {
		t.Errorf("loaded %q, callsign %q", got, f.cfg.Callsign)
	}
}

func TestEmptyFileIsFine(t *testing.T) {
	f := listenFlags()
	if _, err := f.load(t, "agwlisten", writeConfig(t, "")); err != nil {
		t.Fatal(err)
	}
	if f.cfg.HostPort != "localhost:8000" {
		t.Errorf("host = %q, want the default", f.cfg.HostPort)
	}
}
