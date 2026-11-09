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
- `cmd/cursor-relay`, `internal/relay`: the credential-swapping relay for Cursor's own clients (see [Cursor relay](#cursor-relay)).
- `proto`, `internal/gen`: the protocol definitions, extracted from the CLI bundle, and their generated Go types.

## Cursor relay

`cursor-relay` is a small HTTP/2 server for Cursor's **own** clients (`cursor-agent`, `@cursor/sdk`). A client
points its API endpoint at the relay and presents a key of your gateway as its Cursor API key; the relay forwards
every call to Cursor exactly as the client sent it, except the credential, which it replaces with the account's.
The same warning applies as for the plugin: Cursor's terms forbid this use.

- `agent.v1.*` goes to the agent host Cursor announces, `/v1/*` (the public API) to `https://api.cursor.com` with the
  user API key, everything else to `https://api2.cursor.sh` with the account's session token. Bodies, headers other
  than the credential, trailers and bidirectional streams pass unchanged; a client cancel cancels upstream.
- `GetServerConfig` is the one response the relay edits: every agent host it announces is replaced by the relay's
  public URL, so the run stream comes back to the relay instead of going to Cursor directly.
- `POST /auth/exchange_user_api_key` is answered by the relay. It asks your gateway whether the client key is valid
  (one `GET` with the key as bearer, `200` means valid, cached briefly) and returns a token that has the session
  token's JWT header and claims and, in place of the signature, the client key sealed with the relay's own key
  (AES-256-GCM): a restart needs no state, and a revoked key stops working within the cache time. Every other call
  must carry such a token (`/v1/*` carries the client key itself), otherwise it gets `401` and nothing is sent to
  Cursor. No credential a client sends is ever forwarded.
- The relay exchanges the account's user API key for a session token itself, renews it five minutes before it
  expires, and renews it once after an upstream `401` (a unary call is then sent again; a stream is not).
- Logs carry the method, path, status, duration and a short hash of the client key, never a key or a token.
- `GET /healthz` answers `200 {"upstreamToken":true}` while a session token is held, `503` otherwise.

### Configuration

All settings are environment variables, never flags. A secret can be given directly or, with the `_FILE` suffix,
as the path of a file that holds it.

| Variable | Meaning | Default |
|---|---|---|
| `CURSOR_RELAY_USER_API_KEY[_FILE]` | The Cursor user API key of the account (required). | |
| `CURSOR_RELAY_SEAL_KEY[_FILE]` | A secret of at least 32 characters that seals client keys inside tokens (required), for example `openssl rand -base64 32`. Changing it signs every client out. | |
| `CURSOR_RELAY_VERIFIER_URL` | URL asked with `GET` and the client key as bearer; `200` means valid (required). | |
| `CURSOR_RELAY_PUBLIC_URL` | How clients reach the relay, for example `https://relay.example.com` (required). | |
| `CURSOR_RELAY_LISTEN` | Listen address. | `:8080` |
| `CURSOR_RELAY_TLS_CERT_FILE`, `CURSOR_RELAY_TLS_KEY_FILE` | Serve TLS. Without them the relay serves cleartext HTTP/2 (h2c) and HTTP/1.1, for a reverse proxy. | |
| `CURSOR_RELAY_VERIFY_TTL` | How long a valid answer of the verifier is kept. | `1m` |
| `CURSOR_RELAY_VERIFY_NEGATIVE_TTL` | How long a refusal is kept. | `10s` |
| `CURSOR_RELAY_DRAIN_TIMEOUT` | How long open streams may finish after `SIGTERM`. | `30s` |
| `CURSOR_RELAY_API_URL`, `CURSOR_RELAY_AGENT_URL`, `CURSOR_RELAY_PUBLIC_API_URL` | Upstreams, for tests and proxies. | Cursor's |

### Run it

`make relay-image` builds a static, non-root image (`RELAY_IMAGE=name:tag` to name it). Behind a reverse proxy, the
relay must be reached over HTTP/2, because the agent run is a bidirectional stream. With Caddy:

    relay.example.com {
        reverse_proxy h2c://127.0.0.1:8080 {
            flush_interval -1
        }
    }

    CURSOR_RELAY_PUBLIC_URL=https://relay.example.com \
    CURSOR_RELAY_VERIFIER_URL=https://gateway.example.com/v1/models \
    CURSOR_RELAY_USER_API_KEY_FILE=/run/secrets/cursor_user_key \
    CURSOR_RELAY_SEAL_KEY_FILE=/run/secrets/relay_seal_key \
    cursor-relay

Clients then run with the relay as endpoint and a gateway key as API key:

    CURSOR_API_ENDPOINT=https://relay.example.com CURSOR_API_KEY=<gateway key> cursor-agent -p "..."
    CURSOR_BACKEND_URL=https://relay.example.com    CURSOR_API_KEY=<gateway key> node app-using-cursor-sdk.mjs

A few calls of the clients still go to Cursor directly, without any credential: the feature-flag bootstrap
(`BootstrapStatsig` on `api2.cursor.sh`) and Cursor's own MCP servers. The relay sees only what the client sends it.

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
