# ClinePassProxy

Standalone Cline reverse proxy. It replaces the ClinePassBridge CPA-executor data
path so a large model request body crosses exactly one HTTP boundary on its way
to Cline.

```text
pi / Codex / NewAPI #53
        |
        v
  ClinePassProxy  (/v1/messages, /v1/responses, /v1/chat/completions, /v1/alpha/search)
        |
        v   direct HTTP + SSE, no CPA plugin RPC
     Cline API
```

Codex search requests use the optional `/v1/alpha/search` adapter, which
forwards the request to CommandCodeProxy on the shared Docker network. Cline
does not provide this endpoint.

Claude Code's Anthropic `web_search_*` server-tool requests are detected on
`/v1/messages` and forwarded to CommandCodeProxy's Anthropic endpoint. They do
not become ordinary Cline function tools, because Claude Code requires native
`server_tool_use` and `web_search_tool_result` blocks.

```text
CPAMP -> ClinePassProxy Connector (management only) -> ClinePassProxy Management API
```

The connector registers no executor, scheduler, interceptor or translator. It
cannot carry inference traffic by construction.

## Why

CPA builds an executor request that carries both the original client body and the
translated provider body, then serializes and clones it across the plugin RPC
boundary. For large Claude Messages requests that preprocessing alone measured
roughly 5-6 s, on top of the provider's own 4-5 s warm-cache first token. Moving
the same work into a plain HTTP proxy removes that structural stage without
changing the request the provider sees.

## API

```text
POST /v1/messages          Claude Messages  -> Cline Chat Completions -> native Claude SSE
                             (native web_search_* -> CommandCodeProxy /v1/messages)
POST /v1/responses         OpenAI Responses -> Cline Chat Completions -> native Responses SSE
POST /v1/chat/completions  minimal normalization -> Cline SSE / JSON
POST /v1/alpha/search      Codex SearchRequest -> CommandCodeProxy /v1/alpha/search
GET  /health
GET  /ready
```

`/v1/responses` is a protocol bridge, not native upstream Responses passthrough.
Client-executed `tool_search` is bridged end-to-end: the client schema becomes a
Chat function, provider calls return native `tool_search_call` items, and
`tool_search_output` loads discovered namespace/function/custom definitions for
following turns. `additional_tools` is also loaded. Still-deferred definitions
are not exposed before discovery, and ordinary functions named `tool_search`
remain ordinary functions. Streaming and blocking responses share this mapping.
Hosted/server tool search is not implemented by the Cline Chat upstream and is
rejected explicitly. This does not use the independent `/v1/alpha/search` web
search adapter. It preserves existing request-history order; it does not claim
OpenAI's native append-only tool-loading cache behavior for the Chat upstream.

Management API and the web UI live under `/api/*` and `/`.

## Console

The management console is a single embedded document (`internal/serving/web`)
that is delivered with its stylesheets and scripts inlined, so the CPA connector
can fetch `/` once and serve the whole console from the CPA origin. The browser
only ever talks to its own origin; the connector attaches the management token
server-to-server.

```text
Dashboard    service identity, 24h traffic, official quota per credential, recent requests
Credentials  add/edit/delete, key replacement, enable/disable, per-credential proxy, test, quota refresh
Models       alias CRUD (client id -> upstream id), provider allow-list/pinning, real model test with upstream errors
Requests     filterable request viewer with per-request timings, attempts and token accounting
Usage        official Cline quota (5h / weekly / monthly + 31-day totals) and proxy-observed traffic
Settings     base URL, limits, affinity, gateway keys, management credentials, theme
```

Two usage sources are shown side by side and never mixed:

* **Official Cline usage** comes from the account API (`/users/me`,
  `/users/me/plan/usage-limits`, `/users/{id}/usages/daily`, `/users/{id}/balance`),
  the same source the Cline channel monitor plugin used. It covers the whole
  account, including other clients.
* **Observed through ClinePassProxy** is derived from this proxy's own request
  history.

A field the upstream does not return is rendered as `—`; nothing is estimated.
Theme supports System / Light / Dark, defaults to System, follows the CPAMP host
theme when embedded, and persists an explicit choice in the browser.

## Authentication

Three independent credentials, by design:

```text
management token   -> /api/* and the web UI (HTTP Basic Auth or bearer)
gateway API key    -> /v1/* inference endpoints
Cline credential   -> upstream Cline calls only, stored in data/credentials.json
```

The management token is generated on first start and written to
`<data-dir>/config.json`. A gateway key never authenticates management, and a
management token never authenticates inference.

With no gateway key configured the inference endpoints are closed and `/ready`
reports 503. `allow_unauthenticated: true` re-opens them for a deployment where
another component already authenticates callers.

## Configuration

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `CLINEPASSPROXY_DATA_DIR` | `/var/lib/clinepassproxy` | Persistent state directory |
| `CLINEPASSPROXY_LISTEN` | `0.0.0.0:8788` | Listen address |
| `CLINEPASSPROXY_PORT` | `8788` | Port used when `CLINEPASSPROXY_LISTEN` is unset |
| `CLINEPASSPROXY_SEARCH_BASE_URL` | unset | CommandCodeProxy API root for Codex search and Claude native web search |
| `CLINEPASSPROXY_SEARCH_API_KEY` | unset | Optional dedicated CommandCodeProxy data-plane key; when unset, the incoming gateway key is forwarded |

Persistent state:

```text
config.json        settings, gateway keys, management token (0600)
credentials.json   Cline credentials and proxy assignment (0600)
requests.jsonl     bounded request diagnostics (0600)
```

Everything else is editable from the Settings tab: base URL, timeout, response
size limit, retry/failover, affinity policy, log retention, gateway key rotation,
management credentials and the model alias table.

To enable search forwarding, set `CLINEPASSPROXY_SEARCH_BASE_URL` or update
`search_base_url` through the authenticated `PUT /api/config` endpoint. The
normal shared-network value is `http://commandcode-proxy:8787`. Set a separate
`search_api_key` when the two proxies do not share a data-plane key. Codex
requests use `/v1/alpha/search`; Claude Code native `web_search_*` requests use
`/v1/messages` and preserve the Anthropic server-tool response blocks.

An empty alias table means model IDs pass through to Cline unchanged. Once any
alias exists, only listed IDs are accepted, which keeps the proxy fail-closed.

When a model alias has `providers`, CPP sends the allow-list in the routing
shape discovered for that exact upstream model: planner models use
`providerOptions.gateway.only`, while direct OpenRouter models use
`provider.only`. Saved selections without a confirmed pipeline are cleared as
unverified legacy configuration. The actual provider is read from Cline's
response metadata; if it is outside the saved allow-list or a completed pinned
request has no provider evidence, CPP reports an error instead of claiming a
successful pin. The same check applies to the Models page's test button.

The Models page's `POST /api/models/providers/probe` first issues an ordinary
64-token completion to identify the actual pipeline and canonical model. It
then sends a 16-token request with the pipeline-specific invalid `__probe__`
provider to obtain the model's catalog. The ordinary request consumes a small
amount of ClinePass quota. Cline's known `empty response content` error retries
as SSE without raising the token budget or changing the model. Only the returned
model-specific catalog is selectable; history is informational. Editing a
previously confirmed selection preserves it. Leave the list empty for automatic
routing.

`pinning_status` distinguishes `catalog_available`, `routing_ignored`,
`pipeline_unknown` and `probe_unavailable`. If an invalid provider request
completes with another provider, Cline is ignoring the restriction and the UI
shows that the current model cannot be pinned. It does not invent a catalog,
silently accept another provider, or switch to a usage-billed model. Pipeline
and catalog support can change at Cline; a catalog alone is not a successful
pin. Validate a saved selection with a real request and its actual provider.

## Session affinity

Credential selection must be stable: the upstream prompt cache belongs to the
credential that served the previous turn. Identity is derived, in order, from:

1. a configured affinity header,
2. `prompt_cache_key`, `conversation_id`, `session_id`, `thread_id` in the body,
3. common affinity headers (`X-Session-Id`, `X-Conversation-Id`, ...),
4. a hash of the conversation prefix.

Fallback policy is deterministic round-robin. Failover happens only on 401, 403,
402, 429 and 5xx, and only when another credential remains untried.

## Diagnostics

Every request records staged timings (`request_received` through
`request_complete`), prompt/cached/completion/reasoning tokens, cache hit ratio,
time-to-first-token, provider first event, decode-stage TPS and end-to-end TPS.

Read NewAPI's TPS with care: it divides completion tokens by total time, so a
healthy 200 tok/s decode stage can still display a low number when TTFT is large.
The Requests view separates the two.

## Deploy

### Docker Compose (current VPS deployment)

The proxy uses the same deployment structure as CommandCode Proxy: an
Actions-built GHCR image, `restart: unless-stopped`, a persistent bind mount and
an existing NewAPI Docker network. No build runs on the VPS or desktop.

For a fresh deployment, copy `docker-compose.example.yml` to the deployment
directory as `docker-compose.yml`. Set these non-secret values in its private
`.env` file:

```dotenv
CLINEPASSPROXY_IMAGE=ghcr.io/nonlog/clinepassproxy:v0.1.10
CLINEPASSPROXY_NETWORK=backend
CLINEPASSPROXY_DATA_DIR=./data
CLINEPASSPROXY_BIND_IP=127.0.0.1
CLINEPASSPROXY_PORT=8788
```

Use the actual existing network name. The image can also be pinned to a verified
`ghcr.io/nonlog/clinepassproxy@sha256:...` digest. The container listens on
`0.0.0.0:8788` internally; NewAPI on the same network can use
`http://clinepassproxy:8788`. A host-networked management connector uses the
private host-published address instead. Inference never passes through CPA.
The production NewAPI Cline channel (#53) uses exactly
`http://clinepassproxy:8788`; do not point it at the host bridge address when
the caller is the `new-api` container.

The v0.1.10 image runs as UID 100 / GID 101. For a fresh empty state directory:

```bash
install -d -m 0700 -o 100 -g 101 ./data
docker compose config --quiet
docker compose pull
docker compose up -d --no-build --wait
```

For an existing deployment, mount its existing state directory, not an empty
`./data`. Back up the directory first, stop the old instance only when idle,
and grant the image's user ownership while retaining `0700` directories and
`0600` state files. Never run two instances writing the same state directory.
The production migration preserves the previous caller address and gateway/
management/Cline credentials; the legacy systemd unit stays installed but
disabled for rollback. CommandCode Proxy and ClinePassBridge are unchanged.

Before an update or restart, check active requests in the management console
and wait for idle. Then change the pinned image reference, pull, and run
`docker compose up -d --no-build --wait`. The Compose stop grace period is 40
seconds, longer than the application's shutdown deadline; it does not guarantee
that an arbitrarily long generation will survive a restart. Keep the previous
image digest and state backup for rollback.

Either way, open the UI, add at least one Cline credential and set a gateway API
key before pointing a caller at it. Inference stays closed until a gateway key
exists.

Only publish on loopback or a private Docker bridge address, not `0.0.0.0`.
If another service already uses the default host port, choose an unused port
or retain the existing private binding. Shared-network callers use the container
port, independently of the published host port. See
[Docker's external-network documentation](https://docs.docker.com/compose/how-tos/networking/#use-an-existing-external-network).

### Legacy systemd deployment

`scripts/deploy-vps.sh` remains available for systemd-only installations. It
downloads and verifies Actions-built binaries; it refuses to run if a
`clinepassproxy` container already exists, so it cannot accidentally restart the
retired host service alongside the container. Production uses Compose now.

Production artifacts come from GitHub Actions only:

```text
.github/workflows/ci.yml       -> gofmt, vet, race tests, build
.github/workflows/release.yml  -> linux binaries, connector .so, GHCR image, release assets
```

Local and VPS machines deploy those artifacts and may run runtime validation;
they never build release artifacts.

## Tests

```bash
go test ./...
go test -race ./...
cd cpa-connector && go vet ./internal/...
```

The suite covers Claude Messages and Responses conversion, streamed tool calls,
thinking blocks, usage and cache tokens, arbitrary SSE read boundaries, SSE frame
limits, credential affinity stability, failover, request diagnostics and the
separation between management and gateway authentication.

### Translation parity

`internal/translate/testdata/parity/` is a fixture corpus that pins the request
translation rules shared with CPA v8.0.4, one file per rule, each with the reason
the rule matters for the prompt cache. `TestTranslationParityFixtures` compares
the complete translated Chat Completions body, so a shape change fails the test
instead of passing as "semantically equivalent".

Pinned today: Anthropic `system` stays an ordered list of text blocks with the
Claude Code attribution block dropped; parallel Responses calls from one turn
become a single assistant message with every tool result directly after it; tool
calls merge back into the assistant text message that produced them; results
without a `call_id` pair with the calls of the batch they follow, in order; and
an incomplete history is never rewritten.

Not pinned, and therefore not claimed: CPA's rewriting of ambiguous duplicate
call ids, its `[reasoning unavailable]` fallback text, and the Responses
namespace mapping beyond the flatten/restore pair already covered by the stream
converter tests. Adding a rule here means adding a fixture with the reference
behaviour, not just a passing assertion.

## Rollback

Keep ClinePassBridge deployed and keep the previous NewAPI #53 target. Rolling
back is restoring that target; nothing has to be rebuilt.

## Non-goals

The proxy deliberately does not batch SSE frames, buffer a stream before
forwarding, reuse the old simplified native-Claude input conversion, or patch
CPA upstream. Each of those was tried and rejected: batching truncated output,
and the simplified conversion collapsed prompt-cache hits for large histories.
