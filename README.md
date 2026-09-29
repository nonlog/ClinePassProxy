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

With no gateway key configured the inference endpoints are closed and `/ready`
reports 503. `allow_unauthenticated: true` re-opens them for a deployment where
another component already authenticates callers.

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

Two hosting models use the same binary and the same state layout. Pick the one
that matches how the caller reaches the proxy.

### systemd (current VPS deployment)

The Cline caller (CPA) runs with host networking, so the proxy runs as a host
process on loopback and CPA reaches it at `http://127.0.0.1:8788`.

```bash
scripts/deploy-vps.sh v0.1.3
```

The script downloads the published binary, verifies its checksum, swaps
`/opt/clinepassproxy/bin/clinepassproxy`, restarts the unit and checks `/health`
and `/ready`. The unit sets `CLINEPASSPROXY_DATA_DIR=/var/lib/clinepassproxy` and
`CLINEPASSPROXY_LISTEN=127.0.0.1:8788`, and keeps `ProtectSystem=full` with
`ReadWritePaths` limited to the state directory.

### Docker

Use this when the caller is another container on a shared Docker network; the
published image listens on `0.0.0.0:8788` inside its own namespace.

```bash
docker run -d --name clinepassproxy \
  -p 127.0.0.1:8788:8788 \
  -v clinepassproxy-data:/var/lib/clinepassproxy \
  ghcr.io/nonlog/clinepassproxy:latest
```

`docker-compose.example.yml` is the same deployment in compose form.

Either way, open the UI, add at least one Cline credential and set a gateway API
key before pointing a caller at it. Inference stays closed until a gateway key
exists.

A container on the default bridge cannot reach a host process on `127.0.0.1`.
Bind the proxy to the bridge address (`172.17.0.1:8788`) or run it on the
caller's Docker network; the loopback default is deliberate, not a limitation to
work around with `0.0.0.0`.

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
