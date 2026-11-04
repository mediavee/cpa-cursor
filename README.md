# cpa-cursor

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 plugin that adds Cursor as a provider:
requests sent to CPA in any format it speaks are served by a Cursor subscription, through the same
backend protocol as the official `cursor-agent` CLI.

> **Unofficial.** This is not affiliated with or endorsed by Anysphere. Cursor's terms of service forbid
> using a subscription outside its own clients, and Cursor has said such use can lead to an account
> ban. You use this at your own risk.

Status: in development.

## Layout

- `proto/`: the protobuf definitions the plugin uses, extracted from the official CLI bundle.
- `protoref/`: the rest of the extraction, kept for reference and regeneration.
- `internal/gen/`: Go types generated from `proto/` (`make gen`).
- `tools/extract_proto.py`: rebuilds `.proto` files from a CLI bundle.
