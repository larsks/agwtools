# agwtools

A collection of tools for working with AGWPE endpoints.

## Commands

The following examples will all attempt to connect to an AGWPE gateway at `localhost:8000` by default. Use the `-h` option to specify a different `host:port` if necessary.

### agwlisten

`agwlisten` is a little bit like inetd for AGWPE: it listens for incoming connections to a specific callsign, and then launches a command with stdin and stdout attached to the AGWPE connection. For example, to make a shell accessible via AGWPE at callsign SHELL:

```sh
agwlisten -c SHELL -t -- dash
```

Available options:

```
Usage: agwlisten [-f <config>] [-h <agwpe_host:port>] [--pty|-t] [-c <callsign>] [-p <port>] [-m <limit>] [-k <interval>] [-i <timeout>] [--raw|-r] [--once|-o] [--version] -- <command> [<args>...]
  -c, --callsign string         local callsign to register (default "NOCALL")
  -f, --config string           configuration file (default: $AGWTOOLS_CONFIG_PATH, else $XDG_CONFIG_HOME/agwtools/config.toml, else ~/.config/agwtools/config.toml; "" to read none)
  -h, --host string             agwpe_host:port (default "localhost:8000")
  -i, --idle-timeout duration   disconnect a session after this period of inactivity from the remote station (0 to disable) (default 10m0s)
  -k, --keepalive duration      interval for AGWPE keepalive frames, to prevent the server from closing an idle connection (0 to disable) (default 1m0s)
  -m, --max-connections int     maximum simultaneous connections (0 = unlimited)
  -o, --once                    exit after first command completes
  -p, --port int                radio port
  -t, --pty                     allocate a pty for the command
  -r, --raw                     do not translate \r\n to \r in command output sent to the remote station
      --version                 print the build date and git commit, then exit
```

### agwconnect

`agwconnect` is like `telnet` for AGWPE: it allows you to connect to a remote host via an AGWPE gateway:

```sh
agwconnect N0CALL-4
```

Available options:

```
Usage: agwconnect [-f <config>] [-h <agwpe_host:port>] [-c <callsign>] [-p <port>] [-k <interval>] [-v <digipeaters>] [--raw|-r] [-w <seconds>] [--version] <remote-callsign>
  -c, --callsign string      local callsign to register (default "NOCALL")
  -f, --config string        configuration file (default: $AGWTOOLS_CONFIG_PATH, else $XDG_CONFIG_HOME/agwtools/config.toml, else ~/.config/agwtools/config.toml; "" to read none)
  -h, --host string          agwpe_host:port (default "localhost:8000")
  -k, --keepalive duration   interval for AGWPE keepalive frames, to prevent the server from closing an idle connection (0 to disable) (default 1m0s)
  -p, --port int             radio port
  -r, --raw                  do not translate line endings (default: \n <-> \r)
      --version              print the build date and git commit, then exit
  -v, --via strings          digipeater path, comma-separated or repeated (e.g. -v WIDE1-1,WIDE2-1)
  -w, --wait int             after end of input, keep the session open until the remote station has been silent for this many seconds, then disconnect; 0 disconnects at once (default: 30 if stdin is not a terminal, otherwise 0)
```

## Configuration file

Every command option can also be set in a TOML configuration file. The file
is read from, in order of preference:

1. `--config`/`-f`
2. `$AGWTOOLS_CONFIG_PATH`
3. `$XDG_CONFIG_HOME/agwtools/config.toml`
4. `~/.config/agwtools/config.toml`

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
