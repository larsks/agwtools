// Package configtest checks config.example.toml against the commands' real
// options, so the example can't drift from what the commands accept.
package configtest

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/BurntSushi/toml"
	flag "github.com/spf13/pflag"
)

// optionLine matches a commented-out option such as `#host = "x"`, but not
// prose that merely mentions one.
var optionLine = regexp.MustCompile(`(?m)^#([a-z][a-z-]*) = `)

// UncommentedExample writes a copy of config.example.toml, with every
// commented-out option enabled, to a temporary file and returns its path.
func UncommentedExample(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(repoRoot(t), "config.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	enabled := optionLine.ReplaceAll(data, []byte("$1 = "))
	if slices.Equal(enabled, data) {
		t.Fatal("config.example.toml has no commented-out options to enable")
	}

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, enabled, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// RequireCovers fails the test unless every option in flags, other than
// --config, appears in [shared] or in the [command] section of the example at
// path.
func RequireCovers(t *testing.T, flags *flag.FlagSet, command, path string) {
	t.Helper()

	var sections map[string]map[string]any
	if _, err := toml.DecodeFile(path, &sections); err != nil {
		t.Fatal(err)
	}

	flags.VisitAll(func(f *flag.Flag) {
		if f.Name == "config" || f.Name == "help" {
			return
		}
		_, inShared := sections["shared"][f.Name]
		_, inCommand := sections[command][f.Name]
		if !inShared && !inCommand {
			t.Errorf("--%s is not in config.example.toml, in [shared] or [%s]", f.Name, command)
		}
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the working directory")
		}
		dir = parent
	}
}
