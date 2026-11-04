# cpa-cursor

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) v8 plugin that adds Cursor as a provider:
requests sent to CPA in any format it speaks are served by a Cursor subscription, through the same
backend protocol as the official `cursor-agent` CLI.

> **Unofficial.** This is not affiliated with or endorsed by Anysphere. Cursor's terms of service forbid
> using a subscription outside its own clients, and Cursor has said such use can lead to an account
> ban. You use this at your own risk.

Status: usable; tested against CPA v8.0.53 (plugin ABI schema 10).

Never commit tokens, e-mail addresses or account identifiers: tests use invented values.

## Layout

- `proto/`: the protobuf definitions the plugin uses, extracted from the official CLI bundle.
- `protoref/`: the rest of the extraction, kept for reference and regeneration.
- `internal/gen/`: Go types generated from `proto/` (`make gen`).
- `tools/extract_proto.py`: rebuilds `.proto` files from a CLI bundle.
- `internal/cursor/`: the protocol layer (Run session, parking, errors, unary client, login).
- `plugin/`: the CPA plugin itself (ABI messages: auth, models, executor, quota). It is a normal package so it is unit-tested without cgo.
- `cmd/plugin/`: the cgo glue that exports the plugin as a C shared library.
- `cmd/cursor-probe/`: a diagnostic client (`models`, `usage`, `run`) for checking the layer against a real account.

## The CPA plugin

### Build

    make plugin

builds `bin/plugins/linux/arm64/cursor.so` and `bin/plugins/linux/amd64/cursor.so` in `golang:1.26-bookworm`,
the image CPA itself is built in (a plugin must link against a glibc no newer than CPA's). Each architecture is
built by a container of that architecture, so one of them runs under emulation unless you build on a matching
host. `VERSION=...` stamps the version CPA shows (default: `git describe`).

### Install

Put the library where CPA looks for plugins, `<plugins.dir>/linux/<arch>/cursor.so` (the file name is the
plugin id), and enable it in `config.yaml`. The block is opt-in: without `enabled: true` CPA never loads it.

    plugins:
      enabled: true
      dir: "plugins"
      configs:
        cursor:
          enabled: true
          # Everything below is optional.
          client_version: "cli-2026.10.22-27dcf5f"   # the cursor-agent release to impersonate (see below)
          ghost_mode: true                           # ask Cursor not to keep the conversation
          park_ttl: 10m                              # how long a run waits for the client's tool results
          max_parked: 64                             # parked runs kept per credential
          models_refresh_interval: 1h                # how often a credential's model list is refreshed
          request_timeout: 15m                       # upper bound of one request
          api_url: "https://api2.cursor.sh"          # for tests and proxies only
          agent_url: "https://agentn.global.api5.cursor.sh"   # Run stream host, used when Cursor announces none
          site_url: "https://cursor.com"

A change to this block is applied by CPA's config reload, without restarting. `client_version` is sent to
Cursor on every request; when Cursor starts refusing it as outdated the plugin answers `503` with a message that
says so, and the fix is to set the version of a current `cursor-agent` release.

### Connect an account

Three ways, all end in a credential file in CPA's `auth-dir`:

1. **Browser login** (the CLI's PKCE login). `GET /v8/management/oauth/auth-url?provider=cursor` with the
   management key returns a `url` to open on any machine and a `state`; poll
   `GET /v8/management/oauth/status?state=...` until it says `ok`. CPA then writes the credential.
2. **API key.** `GET /v8/management/oauth/auth-url?provider=cursor&api_key=<key>`, then the same status poll. The
   key is exchanged for a session token on the first poll. A query string can end up in a reverse proxy's logs
   (CPA's own request log shows the path only); prefer 3 behind a proxy.
3. **Credential file.** Drop `{"type": "cursor", "api_key": "<key>"}` (or `{"type": "cursor", "access_token":
   "<session token>"}`) in `auth-dir`. A file with a key and no token gets its token at the first refresh.

When a session token nears its end (five minutes before its `exp`) CPA's refresh loop makes the
plugin exchange the stored API key for a new one. A credential that has no key cannot be renewed: when its token
expires, requests fail with `401 ... reconnection required` and the credential must be connected again.

### What it serves

`/v1/chat/completions` in, `/v1/chat/completions` out; CPA translates Anthropic `/v1/messages`, Responses and the
other formats around it. Each credential lists the models of its own account (`/v1/models`), under the slugs
Cursor uses (`composer-2.5`, `claude-sonnet-5-5-medium`, ...). `reasoning_effort` picks the effort variant the
account has for a model. Tools and tool calls, images (`data:` URLs), reasoning (`reasoning_content`) and usage
(in the last chunk of a stream) are supported. `GET .../v0/management/plugins/cursor/quota` reports plan, spend,
Auto and API pools, limit status and the end of the billing cycle.

### Limits to know about

- **Cursor's agent prompt costs tokens.** Every run starts with Cursor's own agent prompt (about 11k input tokens
  on top of yours). `usage.prompt_tokens` includes it, and so does Cursor's billing. Cursor's built-in tools
  (shell, file edits, ...) are refused; only the tools the client sends are available to the model.
- **Tool loops use a parked run.** When the model calls a client tool, the run is parked and the next request
  (with the tool result) resumes it, for `park_ttl`, only **on the same credential**. With several credentials
  turn on `routing.session-affinity: true` in CPA so a conversation stays on one; otherwise the conversation is
  replayed from its messages on the other credential (works, but costs a new run, and its usage restarts).
  Parked runs live in the plugin's memory and are lost when CPA restarts, which also falls back to a replay.
- **A usage limit cools down one model, not the account.** CPA's plugin ABI makes a plugin's `429` cool down
  only the requested model. The plugin sets `retry_after` to Cursor's `reset_at_ms` (the billing-cycle end when
  Cursor gives none), so that model stays out of rotation until the limit resets, but the same exhausted
  credential is still tried for other models. Cursor's short rate limit is answered with a 30 s retry-after.
- **Model names are the credential's own.** Plugin models are not arbitrated against CPA's native ids, so a slug
  that equals a native model of another provider is served by whichever credential CPA picks. Give the Cursor
  credential a `prefix` (an auth-file field CPA supports) with `routing.force-model-prefix: true` to avoid it.
- **`count_tokens` is an estimate** (about 4 bytes per token, without Cursor's agent prompt): Cursor has no counting
  endpoint.
- **Cancellation.** A cancelled stream stops the Cursor run; CPA gives a plugin no cancel signal for a
  non-streaming request, which therefore runs to its end.
- **Images** are only accepted as `data:` URLs; remote image URLs are rejected with a `400`.
- **No per-credential proxy.** The plugin honours `HTTP_PROXY`/`HTTPS_PROXY` of the CPA process, not the
  credential's `proxy_url`.
- **Network from the plugin.** The plugin calls Cursor itself (HTTP/2 streaming does not fit CPA's buffered host
  HTTP client), from the CPA process.
- **Terms of service.** See the notice at the top: using a Cursor subscription through this plugin is against
  Cursor's terms and may get the account banned.

## Development

`make check` is the gate, and what CI runs on pull requests and `main`: gofmt, `go vet`, golangci-lint
(pinned in the Makefile, configured in `.golangci.yml`), `go test -race`, a check that `internal/gen` matches
`proto/`, the build, and the plugin build (`make plugin`). It needs Go (see `go.mod`), network access to fetch
the pinned tools, and Docker able to run both `linux/arm64` and `linux/amd64` images (QEMU for the foreign one).

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

