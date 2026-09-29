# ClinePassProxy — project constraints

Standalone Cline reverse proxy that replaces the ClinePassBridge inference data path.
This file records the hard constraints for every agent working in this repository.

## Architecture invariant

> No large model request body may pass through a CPA plugin executor RPC.

- ClinePassProxy is the inference data plane. It speaks Cline's Chat Completions
  upstream directly over HTTP/SSE.
- `cpa-connector/` is control plane only. It must not register an executor,
  scheduler, request interceptor, stream interceptor, request translator,
  response translator, or model-inference route.
- CPA/CLIProxyAPI must not be in the inference path for NewAPI channel #53 after cutover.

## Protocol correctness

Do not trade protocol correctness for performance. Never:

- coalesce or batch SSE frames (a previous v0.1.9 batching attempt truncated output),
- buffer a full stream before forwarding,
- use simplified native-Claude input conversion as production behavior
  (a real 356k-token history test collapsed cache hits to ~48k),
- truncate output,
- swallow usage, cache-token, tool, reasoning, finish-reason, or cancellation signals,
- patch CPA/CLIProxyAPI upstream.

Prompt cache is a hard requirement. A lower TTFT with a collapsed cache hit rate is a
regression, not an optimization.

## Review findings that must not regress

Each of these was a real defect found in review. They are invariants, not preferences.

- **Prompt-prefix bytes.** Anthropic `system` stays an ordered list of text blocks,
  with the Claude Code attribution block dropped; never join the blocks into one
  string. Responses input is rebuilt into Chat Completions order: one assistant
  message per turn's parallel calls, every present result directly after it, results
  without a `call_id` paired with the batch they follow in input order, and an
  incomplete history left untouched. A new rule needs a fixture in
  `internal/translate/testdata/parity/`, not just a passing assertion.
- **Cancellation.** The upstream request derives from the client request context, so a
  cancelled turn stops the Cline call instead of running to the upstream deadline. A
  client cancel is never recorded as a credential failure.
- **Shared client state.** `upstream.Client` reads and writes its base URL under a
  lock; `dispatch` repoints it on every request while other requests are in flight.
- **No silent truncation.** An SSE frame is only delivered once a blank line closes
  it. A stream cut mid-frame, without a finish reason, or without `[DONE]` produces an
  error frame (`event: error`, `response.failed`, no `[DONE]`) and is recorded as 502.
  A JSON error body is never appended to an open event stream.
- **Affinity.** Rendezvous (highest-random-weight) hashing. A pool change may only move
  the sessions bound to the credential that left the pool.
- **Readiness.** Derived from the live credential pool on every call, never pinned at
  startup. Inference is closed while no gateway key is configured; `allow_unauthenticated`
  is the only way to open it.
- **Metric honesty.** `ttft_ms` is the write that carried the first visible token;
  `provider_ttft_ms` is the provider's first event. A protocol prologue frame is never
  time-to-first-token.
- **Management edits.** An omitted field keeps its stored value; only an explicit value
  changes it (`enabled`, `proxy_url`).
- **Connector panel.** The browser only ever talks to CPA. The connector serves the
  console and forwards its API calls server-to-server, so the management token stays
  server-side and the in-network proxy address never has to resolve from a browser.

## Supported surface

```
POST /v1/messages          -> Cline Chat Completions -> native Claude Messages SSE
POST /v1/responses         -> Cline Chat Completions -> native Responses SSE
POST /v1/chat/completions  -> minimal normalization  -> Cline SSE / non-stream
GET  /health, GET /ready
```

Three independent credentials:

```
management auth != gateway API key != Cline credential
```

## Security

- Never store real Cline credentials in NewAPI channel config.
- Never log or return raw credential values.
- Credential/config state uses restrictive file permissions (`0600` file / `0700` dir).
- No unauthenticated destructive management calls.
- No origin IPs, credentials, tokens, or private infrastructure secrets in this repo.

## Build and release policy

- All production artifacts are built by GitHub Actions. Never build or package
  production artifacts on the VPS or the Windows PC.
- Local machines may inspect sources, edit, diff, run unit tests, deploy
  Actions-produced artifacts, and do runtime validation.
- If Actions cannot produce a required artifact, report the limitation instead of
  silently building locally.

## Git identity

Every commit created by an agent uses exactly:

```
Codex <codex@openai.com>
```

as both author and committer, plus the trailer:

```
Co-authored-by: Codex <codex@openai.com>
```

## Rollback

ClinePassBridge stays deployed and untouched until ClinePassProxy passes parity,
benchmarks, and cache validation. A rollback should require restoring the NewAPI
#53 target, not rebuilding anything.

## Performance interpretation

NewAPI's displayed TPS (`completion_tokens / total_use_time`) hides healthy decode
throughput behind TTFT. Always separate and report:

```
TTFT | decode-stage TPS | end-to-end TPS | total duration
```

Do not declare success because `emit_wait` is low, tiny prompts are fast, the UI
renders, or unit tests pass.
