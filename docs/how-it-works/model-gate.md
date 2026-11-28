# Model Gate

The Model Gate is an HTTP proxy under `/gate/model/` through which a workload calls a model without holding a provider key. It checks the task lease, the grant and the budget, reserves the cost before the call, forwards the request with the key it holds, and settles the real usage afterwards.

Status: built. Inference that a subscription harness runs outside the gate is not counted here ([ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)).

[![A Model Gate call: lease check and reservation in the store, forward to the provider with the key, response passed back, usage settled and audited.](../diagrams/model-gate-sequence.svg)](../diagrams/model-gate-sequence.svg "Open full size")

## Routes

| Path under `/gate/model/` | Provider |
| --- | --- |
| `openai/v1/chat/completions`, `openai/v1/responses` | OpenAI |
| `local/v1/chat/completions`, `local/v1/responses` | A local server (default `127.0.0.1:11434`, no key needed) |
| `anthropic/v1/messages` | Anthropic |
| `gemini/v1beta/models/<model>:generateContent` and `:streamGenerateContent` | Gemini |

Any other path returns 404. Keys come from the secret store under `openai_api_key`, `anthropic_api_key`, `gemini_api_key` and `local_model_api_key`. With `--model-gateway-url` (or `SABLECOURT_MODEL_GATEWAY_URL`) every provider route goes to one external gateway and no key is required. The caller's own `Authorization`, `x-api-key`, `x-goog-api-key` and `Accept-Encoding` headers are dropped: the gate injects the provider's credential and lets its HTTP transport negotiate gzip and decompress replies (including streams) before reading usage. Redirects are refused.

## Order of checks

Each refusal returns an error; the audit rows are listed below.

1. A bearer token with the `sc_task_` prefix must be present, in `Authorization`, `x-api-key` or `x-goog-api-key`, and match an active lease (not revoked, not expired): 401.
2. The workload must be declared (403), must not be a deterministic controller and must hold `modelGate.allowed` (403, `no_model_gate_grant`).
3. If `providers` is listed it must include the route's provider (403, `provider_not_allowed`). An empty list allows all four.
4. If the lease is pinned, because the task's context packet carried learnings, the route's provider must be in the pinned set (403, `provider_not_admitted_with_learnings`). Fallback candidates are narrowed to that set as well ([destination check](context-and-learning.md#destination-check)).
5. The `budgetRef` must resolve to a `Budget` (403, `missing_budget`).
6. The body is read, up to 16 MiB; a longer body is 413 (`request_body_too_large`) and is not cut and sent. The body must be one JSON value or empty, or it is 400 (`request_body_not_json`), because the gate cannot check it for secrets ([request and response bodies](#request-and-response-bodies)). Then a cost is reserved. An exceeded budget is 429 with `budget exceeded: <reason>`; a missing or malformed child ceiling is 403 `invalid_child_budget_ceiling`; any other budget-check error is 500.
7. The provider key must be available (502 otherwise). A key shorter than 8 bytes is refused with 502 and an error that names its secret, never the value, and is not sent upstream.

## Reserve, forward, settle

Before the call the gate estimates the request: input tokens are one quarter of the bytes in the prompt and schema fields (`messages`, `input`, `contents`, `system`, `systemInstruction`, `system_instruction`, `instructions`, `prompt`, `tools`, `functions`, `response_format`, `text`, `tool_config`, `toolConfig`), and output tokens are the requested maximum (`max_tokens`, `max_completion_tokens`, `max_output_tokens`, or the Gemini equivalent) times the number of choices. With no maximum, output is reserved as one token per choice. Cost uses built-in prices per provider, not per model, pricing input at the highest applicable rate (for Anthropic, the 1-hour cache-write rate, which defaults to twice the input rate; for other providers, the higher of the input rate, cache-write rate, and any explicitly configured 1-hour cache-write rate) without assuming a cache-read discount; the highest of the primary and its fallbacks is reserved. Fractional micro-USD amounts round up (`math.Ceil`), so billable usage never rounds down to zero.

One transaction checks the reservation against the workload's `Budget` and records it:

- per task: `perTask.maxInputTokens` (counting uncached input, cache write and cache read tokens together), `maxOutputTokens` and `maxCostUSD`, counting settled usage plus live reservations of that task;
- per day: `daily.maxCostUSD`, counting the workload's settled usage since 00:00 UTC plus all its live reservations;
- a value of zero means no limit for that dimension. A request is refused when usage is already at the limit or when usage plus the reservation would pass it.

A reservation lasts 15 minutes. After a 2xx response the gate settles: it removes the reservation and writes the actual usage (uncached input, cache write, cache read and output tokens) to the ledger, priced with the model named in the request.

Two parsers read response usage:

- `github.com/shadewright-labs/armory/modelusage` reads Anthropic Messages (`anthropic/v1/messages`), OpenAI Chat Completions (`openai/v1/chat/completions` and `local/v1/chat/completions`), and OpenAI Responses (`openai/v1/responses` and `local/v1/responses`), for both complete JSON replies and SSE streams.
- A local parser in `internal/gate/model/usage.go` reads Gemini JSON and SSE replies (`gemini/v1beta/models/<model>:generateContent` and `:streamGenerateContent`). It splits `cachedContentTokenCount` out of `promptTokenCount` as cache reads and bills `thoughtsTokenCount` (thinking tokens) as output alongside `candidatesTokenCount`.

Providers with explicit cache rates in the built-in table use them: Anthropic bills 5-minute cache writes at $3.75/1M, 1-hour cache writes at $6.00/1M (`CacheWrite1hUSD`, configurable per model or provider and defaulting to twice the input rate), and cache reads at $0.30/1M; OpenAI bills cache reads at $1.25/1M. Anthropic 1-hour cache writes (`cache_creation.ephemeral_1h_input_tokens`, capped at each group's `cache_creation_input_tokens` across top-level, compaction and advisor usage) are priced at the 1-hour rate, with remaining cache writes at the 5-minute rate. Any provider or model without a cache-write or cache-read rate in the table prices those tokens at its input rate, never at zero.

Three extra billing rules keep the gate fail-closed on provider usage fields:

- Anthropic `usage.iterations` entries of type `compaction` and `advisor_message` are counted in the charged uncached input, cache write (including 1-hour TTL writes), cache read and output totals. The gate forwards `anthropic-beta` unchanged rather than stripping headers silently.
- Unclassified Anthropic `usage.iterations` entries (`fallback_message`, and entries with a null, missing or unknown type) are added to charged cache-write tokens at the 1-hour TTL rate (the dearest input rate) and to output tokens, keeping total input tokens the same while overcharging unknown entries in the fail-closed direction.
- When an OpenAI reply reports `total_tokens` above the sum of its input and output parts, the gate charges the difference as output tokens (the dearer side), so charged tokens are never below the provider total.

When a stream reaches its final event and trailing bytes follow, the exact final counts are settled. When a stream yields partial usage before breaking or contradicting itself, the gate settles at least the reserved estimate and at least the reported totals with no cache-read discount. When a response reports no usage, omits output counts (for example, count-only embeddings-style replies if such routes are added later; today the gate only serves chat, responses, messages and Gemini `generateContent` routes), carries a non-terminal Responses status (such as `in_progress`), is malformed, has out-of-range counts (including an OpenAI `total_tokens` below the sum of its parts) or exceeds the 8 MiB capture limit, the reserved estimate is charged, so a call is never free. A non-2xx response, or a failed upstream call, releases the reservation and charges nothing. If settlement itself fails after three tries the reservation stays until it expires, so the budget errs high for those 15 minutes. No ledger entry is written for that call, so once the reservation expires its usage no longer counts against any budget.

Streams are copied to the caller as they arrive with a flush per chunk, so streaming is not delayed by accounting. Each chunk first passes through the exact matcher of `internal/redact`, on the primary and the fallback path, and then through the pattern rules line by line ([request and response bodies](#request-and-response-bodies)). The stream matcher keeps the raw body it has not sent yet and sends only what the batch matcher returns for the part up to the last byte that occurs in no registered spelling (and is not CR, LF, a backslash, `r` or `n`), so the bytes sent equal the batch result for the whole body. The rest waits for the next chunk or the end of the body. Output can therefore arrive in larger pieces than upstream sent. If 64 KiB pile up with no such byte, the stream sends the marker and stops. Response headers are matched the same way, and a header whose name shows a held value is dropped. A response that still carries a `Content-Encoding` after the transport is refused with 502, because the matcher cannot read it.

The gate relays only JSON and server-sent events. The upstream response must carry exactly one `Content-Type` naming `application/json` or `text/event-stream`, with no parameter other than a UTF-8 `charset`. The gate does not copy the upstream value: it sets `application/json` or `text/event-stream` itself, adding `; charset=utf-8` when the upstream named that charset. Any other value (HTML, plain text, `application/x-ndjson`, a `+json` suffix type, another charset, a missing or repeated header) is refused with 502 `upstream response content type not supported`, on any status and on the fallback path, and the upstream body is not sent. No provider the gate serves answers with NDJSON: Gemini's `streamGenerateContent` returns a JSON array, or SSE with `alt=sse`. Every response the gate writes, relayed or refused, carries `X-Content-Type-Options: nosniff`; an upstream value of that header is dropped.

### Request and response bodies

Bodies are untrusted in both directions. Exact match of the values the daemon holds (provider keys, leases) is the boundary; the pattern rules of `internal/redact` for other secret shapes (`ghp_`, `sk-ant-`, private key blocks, `"api_key": "..."` and the like) are best effort ([ADR-0015](../adr/0015-redaction-is-defence-in-depth.md)).

- Request: the gate decodes the JSON body and runs each string, member names included, through both. A string member whose name the key-value rule treats as sensitive has its value replaced. Because the body is decoded first, a secret spelled with JSON escapes (`\u0067hp_...`) is seen as the provider would read it. The upstream gets the body encoded again: member order and spacing can change, numbers are kept as written. The reserve is still estimated from the body the workload sent. Request bodies are not logged or stored.
- Response: each chunk passes the exact matcher stream, and its output is held until a line ends; the complete lines then go through both. JSON strings hold no raw line break, so no token crosses a line in a JSON or SSE body. A private key header with no footer yet holds its lines until the footer comes; one never finished is cut at the header. Error bodies from the provider go the same way. The pattern rules see a response as sent, so a secret shape spelled with JSON escapes in a response is not matched by them.
- Bounds: the gate reads at most 64 MiB of one response and holds at most 8 MiB while it waits for a line end. Past either, or when reading the body fails before its end, the relay stops, nothing held is sent and an `error` row (`upstream_response_too_large`, `upstream_response_line_too_long` or `upstream_response_read_failed`) is appended; the status is already sent, so the caller gets a cut body. Usage is settled from the captured bytes as for any other response.
- Model names read from a request or a response are redacted before they reach the budget ledger or the audit row.

A refused encoding or content type is charged like an unreadable usage report: after a 2xx the admitted reserve is settled, because the provider already did the work, and after a non-2xx the reservation is released.

## Fallback

A request routed to `openai` or `local` may be retried once on the other of the two, if the workload's grant lists it and it is configured, when the first call fails at transport level or answers 429, 502, 503 or 504. The response then carries the header `X-Sablecourt-Fallback-Provider` and an audit row `model.fallback`. Anthropic and Gemini requests are not retried elsewhere because their wire formats differ.

## Child budgets

Reservation and settlement are two transactions, one before the upstream request and one after the response; each is atomic, and a failed settlement keeps the reservation until it expires (PR 86). The ceiling check for delegated tasks came with PR 66.

A task with a parent, such as a workflow step after the first, must carry a ceiling (stored with the task) with `maxInputTokens`, `maxOutputTokens` and `maxCostUSD` (at least one, no unknown members, at most 4 KiB). The gate applies it on top of the workload's budget, counting only that task's own usage; a ceiling of zero refuses everything. A missing or malformed ceiling returns 403 `invalid_child_budget_ceiling`, and an exhausted one 429 with `child_input_tokens`, `child_output_tokens` or `child_cost_usd`. A ceiling cannot raise the workload's own limits. Workflow steps after the first have a parent; see [Known gaps](#known-gaps) for what that means today.

## What goes into the audit chain

The gate appends a row for a missing or invalid token, an unknown workload, a missing grant, a provider or budget refusal, a missing key, a failed upstream call, a refused request body (`request_body_too_large`, `request_body_not_json`), a refused response encoding (`encoded_upstream_response`) or content type (`unsupported_upstream_content_type`), a relay stopped at a bound or a failed read (`upstream_response_too_large`, `upstream_response_line_too_long`, `upstream_response_read_failed`), a fallback, a settlement failure and each successful call (model, uncached input, cache write, cache read, output tokens and cost in micro-USD). It appends nothing for an unknown route, a body that fails to read, an internal budget error or a non-2xx answer from the provider, for instance an upstream 400, which is passed to the caller when its content type is allowed.

## Known gaps

What the code does today, where it falls short of the design.

- Workflow steps after the first have a parent task but are created without a budget ceiling, so a Model Gate call from such a step is refused with `invalid_child_budget_ceiling`. This follows from the code and its tests; it has not been run end to end.
- The gate does not check model families. A workload with `grants.modelGate.allowed: true` picks its model per request and may fall back to another provider, so the manifest loader refuses any workflow that uses such a workload as a `role: verifier` step ([ADR-0010](../adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27).
- Pre-call reserve estimation sizes input from prompt and schema bytes in the request body at one token per four bytes and reserves one output token per choice when no output limit is requested. It cannot upper-bound server-side stored context (`previous_response_id`, `conversation`, `cachedContent`) or fields outside the counted list, so a fallback charge to the reserve is not a strict upper bound for requests that rely on stored context or omit output limits.
- Anthropic `advisor_message` iteration tokens are priced at the request model's rate (using the single built-in per-provider Anthropic rate), so calls whose advisor model is dearer than the request model can be undercharged until per-model pricing is built.

## Not built yet

- Subscription harness inference is not metered; ceilings on rounds and wall time bound it instead.
- Per-model prices and configurable pricing; the daemon uses the built-in table.

## In depth

- [Tools and actions](tools-and-actions.md), [tasks](tasks.md), [audit](audit.md)
- [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md), [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)
- Code: `internal/gate/model/gate.go`, `internal/gate/model/bodies.go`, `internal/gate/model/usage.go`, `internal/gate/model/child_budget.go`, `internal/store/budget.go`
