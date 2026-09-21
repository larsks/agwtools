package agwconn

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	flag "github.com/spf13/pflag"
)

const (
	// ConfigPathEnv names the environment variable that sets the default
	// configuration file.
	ConfigPathEnv = "AGWTOOLS_CONFIG_PATH"

	configFlag    = "config"
	versionFlag   = "version"
	sharedSection = "shared"
)

// sharedFlags are the options registered by Config.AddFlags. They may appear
// in the [shared] section of the configuration file as well as in a
// command's own section.
var sharedFlags = []string{"callsign", "host", "keepalive", "port"}

// commandSections are the sections that name a command. Each command reads
// only its own, and ignores the others.
var commandSections = []string{"agwconnect", "agwlisten"}

// LoadConfigFile reads the configuration file, if there is one, and applies
// its settings to the flags in flags. It must be called after flags has been
// parsed, and before the options are used. command names the running command
// and selects which section, besides [shared], applies.
//
// A setting from the file only takes effect for a flag that was not given on
// the command line, and a command's own section overrides [shared]. The
// precedence is therefore: command line, then command section, then [shared],
// then built-in defaults.
//
// The file is chosen by --config, else $AGWTOOLS_CONFIG_PATH, else
// $XDG_CONFIG_HOME/agwtools/config.toml, else ~/.config/agwtools/config.toml.
// Only the last two are optional: it is normal for them not to exist, and
// that is not an error. A file named with --config or the environment
// variable must exist. --config "" reads no file at all.
//
// It returns the path of the file it applied, or "" if there was none.
func LoadConfigFile(flags *flag.FlagSet, command string) (string, error) {
	return loadConfigFile(flags, command, os.Getenv, os.UserHomeDir)
}

func loadConfigFile(flags *flag.FlagSet, command string, getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	path, explicit := resolveConfigPath(flags, getenv, userHomeDir)
	if path == "" {
		return "", nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return "", nil
		}
		return "", fmt.Errorf("config file: %w", err)
	}

	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return "", fmt.Errorf("config file %s: %w", path, err)
	}

	values, err := configValues(flags, command, raw)
	if err != nil {
		return "", fmt.Errorf("config file %s: %w", path, err)
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	for _, key := range keys {
		if flags.Changed(key) {
			continue // the command line wins
		}
		v := values[key]
		s, err := flagString(v.value)
		if err != nil {
			return "", fmt.Errorf("config file %s: [%s] %s: %w", path, v.section, key, err)
		}
		if err := flags.Set(key, s); err != nil {
			return "", fmt.Errorf("config file %s: [%s] %s: %w", path, v.section, key, err)
		}
	}

	return path, nil
}

// resolveConfigPath picks the configuration file. explicit reports whether
// the user named it (with --config or the environment) rather than it being
// a default location, which decides whether a missing file is an error. An
// empty path means no file should be read.
func resolveConfigPath(flags *flag.FlagSet, getenv func(string) string, userHomeDir func() (string, error)) (path string, explicit bool) {
	if flags.Changed(configFlag) {
		path, _ = flags.GetString(configFlag)
		return path, true
	}
	if p := getenv(ConfigPathEnv); p != "" {
		return p, true
	}
	if dir := getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "agwtools", "config.toml"), false
	}
	if home, err := userHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".config", "agwtools", "config.toml"), false
	}
	return "", false
}

type configValue struct {
	value   any
	section string
}

// configValues validates the parsed file and merges the sections that apply
// to command, with the command's own section taking precedence over [shared].
func configValues(flags *flag.FlagSet, command string, raw map[string]any) (map[string]configValue, error) {
	sections := make(map[string]map[string]any)
	for name, v := range raw {
		table, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("option %q must be inside a section such as [%s]", name, sharedSection)
		}
		if name != sharedSection && !slices.Contains(commandSections, name) {
			return nil, fmt.Errorf("unknown section [%s] (expected [%s], %s)", name, sharedSection, sectionList(commandSections))
		}
		sections[name] = table
	}

	values := make(map[string]configValue)

	// Options that belong to a single command are not valid in [shared],
	// even though this command may know about them.
	for _, key := range sortedKeys(sections[sharedSection]) {
		if !slices.Contains(sharedFlags, key) {
			if isOption(flags, key) {
				return nil, fmt.Errorf("[%s]: option %q is specific to %s; set it in [%s] instead", sharedSection, key, command, command)
			}
			return nil, fmt.Errorf("[%s]: unknown option %q (options for this section: %s)", sharedSection, key, strings.Join(sharedFlags, ", "))
		}
		values[key] = configValue{sections[sharedSection][key], sharedSection}
	}

	own := sections[command]
	for _, key := range sortedKeys(own) {
		if !isOption(flags, key) {
			return nil, fmt.Errorf("[%s]: unknown option %q (options for this section: %s)", command, key, strings.Join(optionNames(flags), ", "))
		}
		values[key] = configValue{own[key], command}
	}

	return values, nil
}

// isOption reports whether name is an option that may be set from the
// configuration file: any long flag except the file's own path and those that
// only make sense on the command line.
func isOption(flags *flag.FlagSet, name string) bool {
	return name != configFlag && name != versionFlag && name != "help" && flags.Lookup(name) != nil
}

func optionNames(flags *flag.FlagSet) []string {
	var names []string
	flags.VisitAll(func(f *flag.Flag) {
		if isOption(flags, f.Name) {
			names = append(names, f.Name)
		}
	})
	slices.Sort(names)
	return names
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func sectionList(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "[" + n + "]"
	}
	return strings.Join(quoted, " or ")
}

// flagString renders a TOML value the way it would be typed on the command
// line, so pflag does the parsing and type checking for every option.
func flagString(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case bool:
		return strconv.FormatBool(v), nil
	case []any:
		parts := make([]string, len(v))
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return "", fmt.Errorf("list items must be strings, got %T", e)
			}
			parts[i] = s
		}
		return strings.Join(parts, ","), nil
	default:
		return "", fmt.Errorf("unsupported value of type %T", v)
	}
}
