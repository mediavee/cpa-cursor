# cpa-cursor

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (CPA) v8 plugin that adds **Cursor** as a provider.
Requests sent to CPA in any format it speaks (OpenAI Chat Completions, Responses, Anthropic Messages, …) are
served by a Cursor subscription, through the same backend protocol as the official `cursor-agent` CLI.

> [!WARNING]
> **Unofficial.** This project is not affiliated with or endorsed by Anysphere. Cursor's terms of service forbid
> using a subscription outside Cursor's own clients, and Cursor has said such use can lead to an account ban.
> You use this at your own risk.

- Every model of the account, under Cursor's own slugs (`composer-2.5`, `claude-sonnet-5-5-medium`, …).
- Streaming, reasoning, images (`data:` URLs), client tools and tool calls, usage in the last chunk.
- Tool loops resume the open Cursor run instead of replaying the conversation.
- Usage limits answered as `429` with the time Cursor resets them, so CPA rotates away.
- Plan, spend and pools exposed through CPA's quota endpoint.
- Login from a headless machine (a link to open anywhere), or with a Cursor API key that renews itself.

Tested with CPA v8.0.53 (plugin ABI schema 10), on `linux/arm64` and `linux/amd64`.

## Quick start

1. **Download** the library for your platform from the
   [latest release](https://github.com/mediavee/cpa-cursor/releases/latest) and check it against `SHA256SUMS`.
2. **Install** it where CPA looks for plugins, under a versioned name:

       <plugins.dir>/linux/<arch>/cursor-v<version>.so

3. **Enable** it in CPA's `config.yaml`:

       plugins:
         enabled: true
         dir: "plugins"
         configs:
           cursor:
             enabled: true

4. **Connect** a Cursor account (see [Accounts](#accounts)), for example with a credential file in CPA's
   `auth-dir`:

       {"type": "cursor", "prefix": "cursor", "api_key": "<your Cursor API key>"}

5. **Call** it like any CPA model:

       curl $CPA/v1/chat/completions -H "Authorization: Bearer $CPA_KEY" -H 'Content-Type: application/json' \
         -d '{"model": "cursor/composer-2.5", "messages": [{"role": "user", "content": "Say pong"}]}'

CPA loads the plugin at its next config reload, without a restart.

## Configuration

Every key below `enabled` is optional; the values shown are the defaults.

    plugins:
      configs:
        cursor:
          enabled: true
          client_version: "cli-2026.10.22-27dcf5f"   # the cursor-agent release the plugin presents itself as
          ghost_mode: true                           # ask Cursor not to keep the conversation
          park_ttl: 10m                              # how long a run waits for the client's tool results
          max_parked: 64                             # parked runs kept per credential
          models_refresh_interval: 1h                # how often a credential's model list is refreshed
          request_timeout: 15m                       # upper bound of one request
          api_url: "https://api2.cursor.sh"          # for tests and proxies only
          agent_url: "https://agentn.global.api5.cursor.sh"   # run stream host, used when Cursor announces none
          site_url: "https://cursor.com"

A change to this block is applied by CPA's config reload. When Cursor starts refusing `client_version` as
outdated, requests answer `503` with a message that says so: set the version of a current `cursor-agent` release.

**Updating the plugin.** CPA hot-reloads a plugin only when its file name changes. Add the new release under its
own versioned name, remove the old file, then trigger a config reload. A file overwritten in place stays the old
library until CPA restarts.

## Accounts

Three ways to connect an account; all end in a credential file in CPA's `auth-dir`.

1. **Browser login** (the CLI's PKCE login). With the management key,
   `GET /v8/management/oauth/auth-url?provider=cursor` returns a `url` to open on any machine and a `state`;
   poll `GET /v8/management/oauth/status?state=...` until it says `ok`.
2. **API key.** `GET /v8/management/oauth/auth-url?provider=cursor&api_key=<key>`, then the same poll. A query
   string can end up in a reverse proxy's logs; prefer 3 behind a proxy.
3. **Credential file.** `{"type": "cursor", "api_key": "<key>"}`, or `{"type": "cursor", "access_token":
   "<session token>"}`. A file with a key and no token gets its token at the first refresh.

A session token lasts 60 days. With an API key the plugin renews it five minutes before it expires; without one,
requests then fail with `401 ... reconnection required` and the account must be connected again.

**Prefix.** Cursor serves models whose slugs can equal another provider's (`claude-…`, `gpt-…`), and CPA does not
arbitrate plugin models against its native ones. Give the credential a `prefix` (`"prefix": "cursor"`) and set
`routing.force-model-prefix: true`; clients then ask for `cursor/<slug>`.

**Quota.** `POST /v8/management/plugins/cursor/quota` with `{"auth_index": "<index>"}` (the index CPA lists for
the credential) reports the plan, spend, the Auto and API pools, the limit status and the end of the billing
cycle.

## Limits

- **Cursor's agent prompt costs tokens.** Every run starts with Cursor's own agent prompt, about 11k input tokens
  on top of yours, which a normal account cannot replace. `usage.prompt_tokens` includes it, and so does Cursor's
  billing. Your system prompt is applied on top of it.
- **Only the client's tools run.** Cursor's built-in tools (shell, file edits, …) are refused, and the refusal
  names the client's tools so the model switches to them.
- **Tool loops resume on the same credential.** A run that waits for tool results is parked for `park_ttl`, in
  the plugin's memory. With several credentials, turn on `routing.session-affinity: true` so a conversation stays
  on one; otherwise, or after a CPA restart, the conversation is replayed from its messages (it works, but costs
  a new run).
- **A usage limit cools down one model, not the account.** CPA's plugin ABI lets a plugin's `429` cool down only
  the requested model. Its `retry_after` is the time Cursor resets the limit (the end of the billing cycle when
  Cursor gives none); a short rate limit gets 30 s.
- **`count_tokens` is an estimate** (about 4 bytes per token, without the agent prompt): Cursor has no counting
  endpoint.
- **Cancellation** stops the Cursor run for a stream; CPA gives plugins no cancel signal for a non-streaming
  request, which runs to its end.
- **Images** are accepted as `data:` URLs only; a remote URL is answered `400`.
- **Network.** The plugin calls Cursor itself, over HTTP/2 from the CPA process, and honours its
  `HTTP_PROXY`/`HTTPS_PROXY`, not a credential's `proxy_url`.

## How it works

The plugin speaks `agent.v1.AgentService/Run`, the bidirectional stream the `cursor-agent` CLI uses, with the
CLI's headers and heartbeats. Each request is rebuilt from the client's messages: the history goes into the
conversation state, the system prompt into Cursor rules, and the client's tools are declared as MCP tools of a
`client` server. When the model calls one, the plugin returns the tool calls and keeps the run open; the next
request delivers the results to it.

- `internal/cursor`: the protocol layer: Run session, parking, error classes, unary client, login.
- `internal/translate`: OpenAI Chat Completions to a Cursor run, and Cursor's events back to responses and chunks.
- `plugin`: the CPA plugin (auth, models, executor, quota), a normal package so it is tested without cgo.
- `cmd/plugin`: the cgo glue that exports it as a C shared library.
- `cmd/cursor-probe`: a diagnostic client for a real account.
- `proto`, `internal/gen`: the protocol definitions, extracted from the CLI bundle, and their generated Go types.

## Development

`make check` is the gate CI runs: gofmt, `go vet`, golangci-lint, `go test -race`, a check that `internal/gen`
matches `proto/`, the build and the plugin build. It needs Go (see `go.mod`), network access for the pinned tools,
and Docker able to run `linux/arm64` and `linux/amd64` images.

`make plugin` builds `bin/plugins/linux/{arm64,amd64}/cursor.so` in `golang:1.26-bookworm`, the image CPA is built
in (a plugin must link against a glibc no newer than CPA's). `VERSION=...` stamps the version CPA shows (default:
`git describe`).

`cursor-probe` checks the protocol layer against a real account. It reads the token from `CURSOR_ACCESS_TOKEN`
or `-token-file` and never prints it:

    CURSOR_ACCESS_TOKEN=... go run ./cmd/cursor-probe models
    CURSOR_ACCESS_TOKEN=... go run ./cmd/cursor-probe run -model composer-2.5 -prompt "Say pong"

Tests use invented values: never commit a token, an e-mail address or an account identifier.

### Updating the protocol from a new CLI release

1. Install the official CLI and extract its definitions (the extractor only reads files):

       python3 -I tools/extract_proto.py ~/.local/share/cursor-agent/versions/<version> /tmp/cursor-proto

2. Copy into `proto/` the files `agent/v1/agent_service.proto` imports, transitively, and review the diff of the
   messages the plugin uses.
3. `make gen` rewrites `internal/gen`; commit it with `proto/`. `make check` fails when they disagree.
4. Set `DefaultClientVersion` in `internal/cursor/config.go` to the CLI version the bundle came from.

## License

[MIT](LICENSE)
