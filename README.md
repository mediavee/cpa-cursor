# cpa-cursor

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 plugin that adds Cursor as a provider:
requests sent to CPA in any format it speaks are served by a Cursor subscription, through the same
backend protocol as the official `cursor-agent` CLI.

> **Unofficial.** This is not affiliated with or endorsed by Anysphere. Cursor's terms of service forbid
> using a subscription outside its own clients, and Cursor has said such use can lead to an account
> ban. You use this at your own risk.

Status: in development.

Never commit tokens, e-mail addresses or account identifiers: tests use invented values.

## Layout

- `proto/`: the protobuf definitions the plugin uses, extracted from the official CLI bundle.
- `protoref/`: the rest of the extraction, kept for reference and regeneration.
- `internal/gen/`: Go types generated from `proto/` (`make gen`).
- `tools/extract_proto.py`: rebuilds `.proto` files from a CLI bundle.
- `internal/cursor/`: the protocol layer (Run session, parking, errors, unary client, login).
- `cmd/cursor-probe/`: a diagnostic client (`models`, `usage`, `run`) for checking the layer against a real account.

## Development

`make check` is the gate, and what CI runs on pull requests and `main`: gofmt, `go vet`, golangci-lint
(pinned in the Makefile, configured in `.golangci.yml`), `go test -race`, a check that `internal/gen` matches
`proto/`, and the build. It needs Go (see `go.mod`) and network access to fetch the pinned tools.

`go run ./cmd/cursor-probe` talks to a real account. It reads the access token from `CURSOR_ACCESS_TOKEN`
or from `-token-file`, and never prints it:

    CURSOR_ACCESS_TOKEN=... go run ./cmd/cursor-probe models
    CURSOR_ACCESS_TOKEN=... go run ./cmd/cursor-probe run -model composer-2.5 -prompt "Say pong"

### Regenerating the protocol types from a new CLI bundle

1. Install the official CLI and extract from its version directory (the extractor only reads files):

       python3 -I tools/extract_proto.py ~/.local/share/cursor-agent/versions/<version> /tmp/cursor-proto

2. Put the files `agent/v1/agent_service.proto` imports, transitively, into `proto/`, and the rest of the
   output into `protoref/`. Check that the whole output compiles with `buf build` (module path = the output
   directory) and look at the diff of `proto/` for changes in the messages the plugin uses.
3. `make gen` rewrites `internal/gen`; commit it with `proto/`. `make gen-check` (part of `make check`) fails
   when they disagree.
4. Update `DefaultClientVersion` in `internal/cursor/config.go` to the CLI version the bundle came from.

