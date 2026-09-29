# Translation parity corpus

Each `*.json` file pins one request-translation rule that ClinePassProxy must
keep in common with CPA v8.0.4, because the upstream prompt cache is keyed on
the bytes of the translated prompt. A rule that changes here silently discards
the provider's warm cache, which shows up as a large cache-hit-ratio drop and a
multi-second time-to-first-token regression.

The `note` field names the CPA v8.0.4 behaviour each fixture pins. Sources used
to derive them: `internal/translator/claude/*` (Anthropic Messages to Chat
Completions) and `internal/translator/responses/*` (OpenAI Responses to Chat
Completions), read at tag `v8.0.4`.

`TestTranslationParityFixtures` replays every file. The expected document is the
complete Chat Completions body, so a change in field order or in message shape
fails the test rather than passing as "semantically equivalent".
