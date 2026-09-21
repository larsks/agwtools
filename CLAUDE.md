# ax25-switch

A collection of tools for working with the AGWPE protocol.

## Building and testing

```sh
go build ./...
go test ./...
go fmt ./...
```

## Commands

Implemented:

- `cmd/agwlisten` -- Make a program availble via AGWPE (like inetd for AGWPE)
- `cmd/agwconnect` -- Connect to remote systems (like telnet for AGWPE)

To be implemented:

- `cmd/agwchat` -- AGWPE-based chat program

## Shared packages

- `internal/agwconn` -- Options common to all commands (`--host`, `--port`,
  `--callsign`, `--keepalive`) and a connection to the AGWPE gateway that
  registers our callsign, reads frames, serializes writes and sends keepalives.
  It also loads the TOML configuration file (`LoadConfigFile`, `--config/-f`);
  the format is documented in `README.md`. New options need no extra work:
  keys map to long flag names, and an option registered by `Config.AddFlags`
  is shared, anything else is specific to the command that registers it.
- `internal/fakeagw` -- In-process fake AGWPE gateway for tests.
- `internal/configtest` -- Test helpers that check `config.example.toml`
  against each command's real options. When adding an option, add it to the
  example (commented out, as `#name = value`) or `TestExampleConfig` fails.

## Protocol details

- <https://www.on7lds.net/42/sites/default/files/AGWPEAPI.HTM>
