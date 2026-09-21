# agwtools

A collection of tools for working with AGWPE endpoints.

## Configuration file

Every command option can also be set in a TOML configuration file. The file
is read from, in order of preference:

1. `--config`/`-f`
2. `$AGWTOOLS_CONFIG_PATH`
3. `$XDG_CONFIG_HOME/agwtools/config.toml`
4. `~/.config/agwtools/config.toml`

It is normal for the last two not to exist, and that is not an error. A file
named with `--config` or `$AGWTOOLS_CONFIG_PATH` must exist. `--config ""`
reads no file at all.

Keys are the long option names, and go in one of three sections:

- `[shared]` holds the options common to all commands: `host`, `port`,
  `callsign` and `keepalive`.
- `[agwlisten]` and `[agwconnect]` hold that command's own options, and may
  also override the shared ones. An option that only one command understands
  (such as `pty`) is an error anywhere but that command's section.

A command ignores the other commands' sections. Precedence, highest first, is
the command line, the command's section, `[shared]`, then the built-in default.

A fully annotated example that lists every option is in
[`config.example.toml`](config.example.toml). Copy it and uncomment what you
need.

```toml
[shared]
host = "radio0.local:8000"
callsign = "N0CALL-1"
keepalive = "60s"          # durations are strings

[agwlisten]
idle-timeout = "10m"
pty = true
eol = true

[agwconnect]
via = ["WIDE1-1", "WIDE2-1"]   # a list, or one comma-separated string
wait = 10                      # seconds
raw = false
```
