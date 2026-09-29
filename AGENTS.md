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
