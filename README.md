# ClinePassProxy

Standalone Cline reverse proxy. It replaces the ClinePassBridge CPA-executor data
path so a large model request body crosses exactly one HTTP boundary on its way
to Cline.

```text
pi / Codex / NewAPI #53
        |
        v
  ClinePassProxy  (/v1/messages, /v1/responses, /v1/chat/completions)
        |
        v   direct HTTP + SSE, no CPA plugin RPC
     Cline API
```

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
POST /v1/responses         OpenAI Responses -> Cline Chat Completions -> native Responses SSE
POST /v1/chat/completions  minimal normalization -> Cline SSE / JSON
GET  /health
GET  /ready
```

Management API and the web UI live under `/api/*` and `/`.

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

## Configuration

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `CLINEPASSPROXY_DATA_DIR` | `/var/lib/clinepassproxy` | Persistent state directory |
| `CLINEPASSPROXY_LISTEN` | `0.0.0.0:8788` | Listen address |
| `CLINEPASSPROXY_PORT` | `8788` | Port used when `CLINEPASSPROXY_LISTEN` is unset |

Persistent state:

```text
config.json        settings, gateway keys, management token (0600)
credentials.json   Cline credentials and proxy assignment (0600)
requests.jsonl     bounded request diagnostics (0600)
```

Everything else is editable from the Settings tab: base URL, timeout, response
size limit, retry/failover, affinity policy, log retention, gateway key rotation,
management credentials and the model alias table.

An empty alias table means model IDs pass through to Cline unchanged. Once any
alias exists, only listed IDs are accepted, which keeps the proxy fail-closed.

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

```bash
docker run -d --name clinepassproxy \
  -p 127.0.0.1:8788:8788 \
  -v clinepassproxy-data:/var/lib/clinepassproxy \
  ghcr.io/nonlog/clinepassproxy:latest
```

Then open the UI, add at least one Cline credential, set a gateway API key, and
point NewAPI channel #53 at `http://clinepassproxy:8788`.

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

## Rollback

Keep ClinePassBridge deployed and keep the previous NewAPI #53 target. Rolling
back is restoring that target; nothing has to be rebuilt.

## Non-goals

The proxy deliberately does not batch SSE frames, buffer a stream before
forwarding, reuse the old simplified native-Claude input conversion, or patch
CPA upstream. Each of those was tried and rejected: batching truncated output,
and the simplified conversion collapsed prompt-cache hits for large histories.
