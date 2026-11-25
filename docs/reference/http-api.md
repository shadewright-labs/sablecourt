# HTTP API reference

Every route that `sablecourtd` registers, grouped by the credential that reaches it, taken from the route registrations in `cmd/sablecourtd` and `internal/ui`. Errors are JSON of the form `{"error": "..."}` with these exceptions: `POST /login`, `POST /logout` and `GET /metrics` reply in plain text, an unauthenticated browser page request gets a small HTML page with status 401, `POST /a2a` replies with JSON-RPC error objects, and a stream route that cannot flush replies 500 `streaming unsupported` in plain text.

Status: built.

## Credential classes

Source: `cmd/sablecourtd/operator_auth.go`, `internal/integrations/shadewright.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`.

| Class | Credential | Reaches |
| --- | --- | --- |
| Public | None. The handler may apply its own check. | `GET /login`, `POST /login`, `GET /healthz`, the agent card, static assets, and the two inbound routes in [Inbound integrations](#inbound-integrations-parked). |
| Operator | `Authorization: Bearer op_...`: the contents of the `operator.token` file, exactly 67 characters (`op_` and 64 hex digits). Or the `sablecourt_session` cookie that `POST /login` sets, which counts only when the request has no `Authorization` header at all. | Every route not listed under another class, including `GET /metrics` and `POST /logout`. |
| Observer | `Authorization: Bearer` with the `observer_bearer_token` secret, at most 512 bytes, not starting with `op_` or `sc_task_`. `GET` only. | `GET /api/v1/observer/snapshot`, `GET /api/v1/observer/stream`, `GET /api/v1/tasks/{id}/scry`. The operator credential also works on these. |
| Integration | `Authorization: Bearer` with the `shadewright_bearer_token` secret. | The two `/api/v1/integrations/shadewright/` routes only. On any other route this token is refused with 401. |
| Task lease | `Authorization: Bearer sc_task_...`, minted per task and valid for one hour. The Model Gate also accepts `x-api-key` and `x-goog-api-key`. | Every route under `/gate/`. |

A request with none of these gets 401 `operator credentials required` with a `WWW-Authenticate: Bearer realm="sablecourt"` header. A `GET` or `HEAD` outside `/api/` and `/ui/` that accepts `text/html` gets a 401 HTML page with a link to `/login` instead.

## Request rules

Source: `cmd/sablecourtd/operator_auth.go`, `cmd/sablecourtd/host.go`, `cmd/sablecourtd/main.go`.

| Rule | Value |
| --- | --- |
| Host check | The `Host` header must be `localhost`, `127.0.0.1` or `[::1]` with the listen port, or an entry from `--allow-host`. Anything else is 403 `request host is not allowed`. Over the Unix socket the host is `unix`. |
| Gate host | `/gate/` paths are also accepted on the `--gate-host` authority (default `host.docker.internal:<port>` when listening on loopback) and on the authority that host coding agents use, which is the listen address (`127.0.0.1` for a wildcard listener). A wildcard listener needs `--gate-host`. The council virtual host `sablecourt-council-gate` is never served on a listener: 403 `council gate route is not allowed`. |
| Path form | A path that is not in canonical form (dot segments, doubled slashes) is 401 `canonical request path required`. |
| Cross-origin | A write (any method but `GET`, `HEAD` and `OPTIONS`) that carries `Origin` or `Sec-Fetch-Site` must be same-origin, or it is 403 `cross-origin request blocked`. This check skips `/gate/`, `POST /api/v1/hooks/github` and `POST /a2a`. A write that authenticates with only the session cookie must be same-origin, or it is 403 `same-origin request required`. Bearer calls without those headers pass. |
| Body size | 2 MiB on every write, except `/gate/model/` (16 MiB read) and `POST /api/v1/voice/transcribe` (24 MiB). `POST /api/v1/councils` is read with a 1 MiB cap inside the 2 MiB one. |
| Response headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and a `Content-Security-Policy` of `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; media-src 'self' blob:; img-src 'self' data:; form-action 'self'`. Responses to authenticated routes also carry `Cache-Control: no-store`. |
| Session cookie | `sablecourt_session`: `Secure`, `HttpOnly`, `SameSite=Strict`, path `/`, 30 minutes. Stateless: the value is an expiry time and an HMAC over the expiry and the lowercased `Host`, keyed with a random key held in memory. A restart or `POST /logout` changes the key and ends every session. |
| Login | `POST /login` takes a form (`application/x-www-form-urlencoded`) with exactly one field `token`, at most 1 KiB. Failures are counted per client address (the IPv4 address, or the IPv6 /64 prefix): 10 per minute, and 1024 per minute across all clients. Over a limit the reply is 429 `too many login attempts` with `Retry-After`. A correct token is never counted and always passes. |

## Public routes

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `internal/ui/ui.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | Liveness. Returns 200 `{"status":"ok"}`. | built |
| `GET /login` | HTML sign-in form. | built |
| `POST /login` | Exchange the operator token for a session cookie. 303 to `/` on success. Plain-text errors: 400 `provide one operator token` (or `invalid login form`), 401 `invalid operator token`, 403 `same-origin request required` (over plain HTTP the request must carry an `Origin` header), 413 `login form too large`, 415 `form data required`, 429 `too many login attempts`. | built |
| `GET /.well-known/agent-card.json` | Agent card that lists every workload outside the `sandbox` zone. | parked |
| `GET /favicon.ico`, `GET /apple-touch-icon.png`, `GET /assets/` | Dashboard static files. | built |

## Operator routes

### Tasks

Source: `cmd/sablecourtd/main.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/workloads` | List workload records. | built |
| `GET /api/v1/tasks` | List tasks. | built |
| `POST /api/v1/tasks` | Submit a task. Body `{"workload", "idempotencyKey", "inputJson"}`. 201 with the task, 400 on any refusal. | built |
| `GET /api/v1/tasks/{id}` | The task, its state events and its budget totals as `{"task","events","budget"}`. 404 if unknown. | built |
| `POST /api/v1/tasks/{id}/cancel` | Cancel a task. 200 `{"status":"cancelled","id"}`, or 409 if it cannot be cancelled. | built |
| `POST /api/v1/tasks/{id}/retry` | Submit a new task from the input of an old one. With `?handoff=true` (or `1`) it attaches a failure handoff. 201 with the new task, 404 if unknown, 400 otherwise. | built |
| `POST /api/v1/tasks/{id}/suspend` | Checkpoint a working task. 200 `{"status":"suspended","id"}` or 409. | built |
| `POST /api/v1/tasks/{id}/resume` | Resume a suspended task. 200 `{"status":"working","id"}` or 409. | built |
| `GET /api/v1/tasks/{id}/scry` | Server-sent stream of redacted task output (Scry): an `history` event, then `chunk` events, then `closed`. 404 if the task has no live session. Observer credential accepted. | built |
| `POST /api/v1/tasks/{id}/takeover` | Take exclusive stdin control. Body `{"operator"}`, which defaults to `operator`. 200 `{"status":"takeover","id","operator"}`, 409 if held or the process takes no stdin, 404 if no session. | parked |
| `POST /api/v1/tasks/{id}/stdin` | Write to the task's stdin. Body `{"operator","data"}`. 200 `{"status":"written","bytes"}`, 400 on bad JSON, 409 if another operator holds the takeover, 404 if no session. | parked |
| `POST /api/v1/tasks/{id}/detach` | Release the takeover. Body `{"operator"}`. Always 200 `{"status":"detached","id"}`. | parked |
| `GET /api/v1/stream` | Server-sent `state` event, sent at once and then every 3 seconds, with a fingerprint of task count, pending approval count, newest audit ID and newest task update time. | built |
| `GET /api/v1/budget` | Token and micro-USD totals over all tasks. | built |

### Approvals and devices

Source: `cmd/sablecourtd/main.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/approvals` | List approvals, newest first. `?status=pending`, `approved`, `denied` or `expired` filters; no filter lists all; any other value matches nothing. | built |
| `POST /api/v1/approvals/{id}/approve` | Approve after the out-of-band dialog. Body `{"expectedSha256","decidedBy"}`; `expectedSha256` is optional today. With both `deviceId` and `signature` it takes the device path instead. 200 with the approval, or 409 on any refusal. | built |
| `POST /api/v1/approvals/{id}/deny` | Deny. Body `{"decidedBy","reason"}`; `decidedBy` defaults to `operator`. 200 with the approval, or 409. | built |
| `POST /api/v1/devices/pair/start` | Always 503 `device enrollment requires independent confirmation`. | disabled |
| `POST /api/v1/devices/pair/complete` | Always 503, same message. | disabled |
| `GET /api/v1/devices` | List paired devices. | built |
| `POST /api/v1/devices/{id}/revoke` | Revoke a device. 200 `{"status":"revoked","id"}`, 404 if unknown. | built |

### Audit, memory, manifests and providers

Source: `cmd/sablecourtd/main.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/audit` | The 50 newest audit rows, newest first. | built |
| `GET /api/v1/audit/verify` | Walk the hash chain. 200 `{"valid":true,"verifiedRows":n}`, or 409 with `"valid":false` and the error. | built |
| `GET /api/v1/audit/export` | Up to 10000 newest rows as `application/x-ndjson`, oldest first. | built |
| `GET /api/v1/memory` | List Court Memory entries, at most 100. Query `namespace` and `q` (substring match). Namespaces that start with `learning:` are hidden. | built |
| `POST /api/v1/memory` | Set an entry. Body `{"namespace","key","value"}`. The namespace defaults to `default`; the value is redacted. 201. 403 for a `learning:` namespace, 400 without a key. | built |
| `DELETE /api/v1/memory/{namespace}/{key}` | Delete an entry. 404 if it fails, 403 for a `learning:` namespace. | built |
| `GET /api/v1/schedules` | List declared Schedules. | built |
| `POST /api/v1/schedules/{name}/run` | Run a Schedule now. 200 `{"status":"triggered","schedule"}`, 404 if unknown, 409 if the run is refused, 503 if no scheduler or no manifests are loaded. | built |
| `POST /api/v1/reload` | Reload the manifests. 200 `{"status":"reloaded"}`, 400 with the loader error, 501 if reload is not configured. | built |
| `POST /api/v1/promote/{name}` | For a `sandbox` workload, run `git branch sablecourt/promote-<name>` in the daemon's working directory (not `--repo-root`). 200 `{"status":"branch_created","workload","branch","next"}`. 404 if the workload is unknown, 409 if it is not in the sandbox zone or `git branch` fails (for example the branch exists), 503 if no manifests are loaded. | built |
| `GET /api/v1/providers` | Provider connection status and cached models. An empty list when there is no provider registry. | built |
| `POST /api/v1/providers/{id}/sync` | Refresh a provider's model list and return it. 502 on upstream failure, 503 if there is no provider registry. | built |
| `GET /api/v1/providers/tiers` | The `fast`, `coding` and `critic` model routes. | built |
| `POST /api/v1/providers/tiers` | Save routes. Body is a list of `{"tier","provider","model"}`; entries with another tier are skipped. 200 with the three routes, 400 on bad JSON, 500 if the store fails. | built |
| `GET /api/v1/learnings` | Learnings from Court Memory that match `?repo=` and `?path=` (comma-separated), at most 100. | built |
| `POST /api/v1/learnings` | Always 410 `legacy learning publication is disabled`. | disabled |
| `POST /api/v1/notify/test` | Send a test chat alert. 200 `{"status":"sent"}`, 502 if the webhook is unreachable. | parked |

### Metrics and sign-out

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `internal/telemetry/metrics.go`. Both need the operator credential.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /metrics` | Prometheus text format 0.0.4, see [metrics](metrics.md). Content type `text/plain; version=0.0.4`. A store failure is a plain-text 500. | built |
| `POST /logout` | End every dashboard session: the daemon replaces its session signing key, clears the cookie and answers 303 to `/login`. The request must be same-origin (it needs `Origin` or `Sec-Fetch-Site: same-origin`), otherwise 403 `same-origin request required` in plain text, so a bare bearer call is refused. | built |

### Voice

Source: `cmd/sablecourtd/main.go`, `internal/voice/voice.go`. All voice routes are parked. `POST /api/v1/conclaves/{id}/voice-decree` is listed with the Conclave routes.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/voice/status` | The resolved speech settings as JSON: `sttUrl`, `sttModel`, `ttsBackend` (`http`, `macos-say` or `unconfigured`), `ttsUrl`, `ttsModel`, `ttsVoice`. | parked |
| `POST /api/v1/voice/transcribe` | Speech to text. The body is raw audio (file name `audio.webm`, optional `?language=`) or `multipart/form-data` with a `file` part and an optional `language` field; at most 24 MiB. 200 `{"text"}` with the redacted transcript, 400 on a bad or empty upload, 413 `audio payload too large`, 502 if the speech backend fails. Writes an audit row. | parked |
| `POST /api/v1/voice/speak` | Text to speech. Body `{"text","voice"}`; the text is redacted and cut to 4000 bytes. 200 with the audio and the backend's content type (`audio/wav` by default), 400 on bad JSON or empty text, 502 if the backend fails. | parked |

### Councils and conclaves

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/councils.go`, `cmd/sablecourtd/councils_detail.go`. A Conclave is a multi-agent room; a council is a Conclave opened with a bounded question.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `POST /api/v1/councils` | Open a council. See the body table below. 201 with the session, or 200 with the existing session when `Idempotency-Key` repeats an identical request. 400 on a bad request, 409 on a reused key with a different request, 413 over 1 MiB. | built |
| `GET /api/v1/conclaves` | List Conclave sessions. | built |
| `POST /api/v1/conclaves` | Open a Conclave from a template. Body `{"template","title","decree"}`. 201, 400 on refusal. | built |
| `GET /api/v1/conclaves/{id}` | `{"conclave","messages","relics"}`. For a council, each answer message carries its `round`. 404 if unknown. | built |
| `GET /api/v1/conclaves/{id}/stream` | Server-sent `state` event every 2 seconds with a fingerprint of state, message and Relic counts. | built |
| `POST /api/v1/conclaves/{id}/decree` | Operator Decree. Body `{"to","body"}`. 201 with the message, 409 if refused. | built |
| `POST /api/v1/conclaves/{id}/voice-decree` | Audio Decree, transcribed by the voice backend. The audio is raw or `multipart/form-data` as for `voice/transcribe`, with the 2 MiB write cap. Target from `?to=` or a `to` form field, default `all`. 201 `{"text","message"}`, 409 if refused, 413, 502. | parked |
| `POST /api/v1/conclaves/{id}/close` | Close. Body `{"state"}`, `completed` or `cancelled`; any other value means `completed`. 200 `{"status":"closed"}` or 409. | built |
| `POST /api/v1/conclaves/{id}/relics` | Store a Relic (a named, redacted artifact). Body `{"name","contentType","body"}`; `contentType` defaults to `text/plain`; a repeated name replaces the Relic. 201, 400 with no name or on a refusal. | built |
| `GET /api/v1/conclaves/{id}/relics/{name}` | Read a Relic. 404 if unknown. | built |

`POST /api/v1/councils` body, from `decodeCouncilOpen`. Unknown fields, `null` values and wrong types are all `invalid council JSON object or field`.

| Field | Type | Default | Rule |
| --- | --- | --- | --- |
| `question` | string | none | Required. At most 30 KiB (30720 bytes), no NUL, and the packed opening must fit 59 KiB encoded. |
| `decree` | string | none | If present, must equal `question` after trimming. |
| `title` | string | `Council` | At most 512 bytes. |
| `repo` | string | none | Required absolute path, at most 4096 bytes, of a primary Git checkout owned by the daemon user, outside the state and credential directories. |
| `ref` | string | none | Branch, tag, `HEAD` or full commit ID, at most 256 bytes. Excludes `live`. |
| `live` | bool | `true` when neither `ref` nor `writer` is set | Read-only view of the working tree. Excludes `ref` and `writer`. |
| `writer` | string | none | `claude`, `codex` or `agy`. With no `ref`, the ref is `HEAD`. Reachable through the API only, `sablectl council` has no flag for it. |
| `rounds` | integer | `2` | At least 1. |
| `starts` | integer | `6` | From 1 to 6. |
| `durationSeconds` | integer | `900` | At least 1. |
| `with` | list of strings | `claude`, `codex`, `agy` | Must list all three once each, in any order. |
| `sequential` | bool | `false` | Run the members of a round one after another. |

Header `Idempotency-Key`: at most 256 characters, no control characters, one header only, and no operator token or secret in it. A repeat with a different body is 409. Opening also needs the three harness executables on the daemon's path and a working Seatbelt sandbox (503 otherwise), and is 503 `council opening requires Sanctum` in Citadel mode. The checkout preflight has a 10 second limit.

### Dashboard pages

Source: `internal/ui/ui.go`. The web dashboard uses the operator credential and the same-origin rule. Pages are `GET /`, `/tasks/{id}`, `/providers`, `/studio`, `/conclaves`, `/conclaves/{id}`, `/schedules`, `/audit` and `/memory`. Form posts are under `/ui/`: `tasks` (and `tasks/{id}/` `retry`, `cancel`, `suspend`, `resume`, `messages`), `approvals/{id}/` `approve` and `deny`, `conclaves` (and `conclaves/{id}/` `relics`, `learning`, `decree`, `close`), `memory/` `set` and `delete`, `providers/` `tiers` and `providers/{id}/` `connect`, `disconnect`, `sync`, `models`, `schedules/{name}/run`, `workflows/{name}/run`, and `studio/` `agent`, `workflow`, `conclave`. All are built.

## Observer routes

Source: `cmd/sablecourtd/main.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/observer/snapshot` | `{"source","observedAt","activeTasks","conclaves","pendingApprovals","budget"}`. Active tasks are `submitted`, `working`, `input-required` or `suspended`, with id, workload, state and update time only. | built |
| `GET /api/v1/observer/stream` | Server-sent `observer` event every 3 seconds with a count fingerprint. | built |

## Integration routes

Source: `internal/integrations/shadewright.go`. Both are `POST`, take a JSON object of at most 2 MiB, and record their result in the matching Conclave. Status codes: 200 on a retry that matches the stored record, 201 on first write, 400 invalid input, 401 bad token, 404 unknown Conclave, 409 conflict, 413 too large, 503 unavailable.

| Path | Body fields | Purpose |
| --- | --- | --- |
| `/api/v1/integrations/shadewright/incidents` | `incidentId` (required, `[A-Za-z0-9][A-Za-z0-9_-]{0,127}`), `summary` (required), `conclaveTemplate` (or the `--incident-conclave-template` default), `cluster`, `namespace`, `workload`, `evidence` (object), `logs` | Open a Conclave for an incident and store redacted evidence as a Relic. |
| `/api/v1/integrations/shadewright/verifications` | `conclaveId` and `status` (required, at most 64 printable characters), `incidentId`, `prNumber`, `summary`, `report` (object) | Store a verification report as a Relic of that Conclave. The status text is free, not a fixed list. |

## Task-lease routes

Source: `cmd/sablecourtd/main.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`. The lease must be active: known by its token hash, not expired and not revoked. For `/gate/mcp`, `/gate/actions/`, `/gate/task/` and `/gate/conclave/` the task must also be `working` or `input-required`. A missing or unusable lease is 401, for example `bearer task token required` or `invalid or expired task token`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `POST /gate/model/openai/v1/chat/completions`, `POST /gate/model/openai/v1/responses`, `POST /gate/model/local/v1/chat/completions`, `POST /gate/model/local/v1/responses` | Model Gate to an OpenAI-compatible endpoint. | built |
| `POST /gate/model/anthropic/v1/messages` | Model Gate to Anthropic. | built |
| `POST /gate/model/gemini/v1beta/models/{model}:generateContent`, `...:streamGenerateContent` | Model Gate to Gemini. | built |
| `GET`, `POST`, `DELETE /gate/mcp` | MCP over streamable HTTP, stateless. Lists only the tools granted to the workload and registered in the gate. | built |
| `POST /gate/actions/{action}` | Typed action from the closed catalog (`git.create_branch`, `git.open_pr`, `sandbox.propose_promotion`). Body `{"approvalId","payload"}`, unknown fields refused. Other methods 405. 200 with the result, 202 `{"status":"approval_required","approval_id","payload_sha256"}` when approval is needed, 400 on a bad body, 403 on a policy deny or an approval that does not match, 404 for an unknown action, 502 if the action fails, 503 if the approval queue is missing. | built |
| `GET /gate/task/messages` | Messages of the lease's task. | built |
| `POST /gate/task/messages` | Add a message. Body `{"role","content"}`; `content` is required (400), the role `system` is 403, the stored role is `agent`. 201 `{"status":"recorded","taskId"}`. | built |
| `POST /gate/conclave/{id}/messages` | Post a proposal as the lease's seat. 201 with the message, 403 if not allowed, 409 if refused. | built |
| `GET /gate/conclave/{id}/messages` | Read messages visible to the seat. Query `since` (non-negative integer, else 400). 403 if not allowed. | built |
| `POST /gate/conclave/{id}/relics` | Store a Relic as the seat. Body as for the operator route. 201, 403, 400. | built |
| `GET /gate/conclave/{id}/relics/{name}` | Read a Relic the seat may see. 403 if not allowed, 404 if unknown. | built |

Any other path under `/gate/model/` is 404 `unknown model gate route`. A workload with no Model Gate grant is 403. A provider outside a non-empty `providers` list is 403. Exhausted budget is 429 `budget exceeded: <reason>`. A 429, 502, 503 or 504 from `openai` or `local` (or a failed connection) falls back to the other of the two when the workload lists it in a non-empty `providers` list, and the response carries `X-Sablecourt-Fallback-Provider`.

### Council gate allow-list

Source: `internal/gate/councilproxy/proxy.go`. A council container reaches the daemon only through its own gate, and the gate forwards only these requests: `GET`, `POST`, `DELETE /gate/mcp`; `GET`, `POST /gate/task/messages`; `POST /gate/actions/{action}`; `GET`, `POST /gate/conclave/{id}/messages`; `POST /gate/conclave/{id}/relics`; `GET /gate/conclave/{id}/relics/{name}`. It never forwards `/gate/model/`. It also opens `CONNECT` tunnels on port 443 to the provider hosts of its own harness.

## Inbound integrations (parked)

Source: `cmd/sablecourtd/main.go`, `internal/webhook/github.go`, `internal/a2a/handler.go`. These two routes skip the operator check and authenticate themselves.

| Method and path | Credential | Purpose | Status |
| --- | --- | --- | --- |
| `POST /api/v1/hooks/github` | HMAC SHA-256 in `X-Hub-Signature-256` with the `github_webhook_secret` secret | For an `issues` event with action `opened`, submit a task to the workload `triage-agent`. Other events or actions return 200 `ignored`. 202 `{"status":"dispatched","taskId"}` on dispatch, 401 on a missing secret or bad signature, 409 if the task is refused, 400 on bad JSON. | parked |
| `POST /a2a` | Bearer `a2a_bearer_token` secret | JSON-RPC 2.0 methods `tasks/send` (needs `workload`) and `tasks/get` (needs `id`). Errors are JSON-RPC error objects with HTTP 200, except a missing or wrong token, which is HTTP 401 with an error object. | parked |

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: passkey enrolment, per-session revocation and a single-use `consumed` state for approvals. None of them exists on `main`. `POST /logout` ends all sessions at once by replacing the signing key; a single session cookie cannot be revoked on its own.

Decided in [ADR-0012](../adr/0012-learning-evidence-and-expiry.md): operator acceptance and scoped matching of learnings. `POST /api/v1/learnings` stays disabled until then.

## In depth

- [Identity and access](../how-it-works/identity-and-access.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md), [approvals](../how-it-works/approvals.md), [conclaves](../how-it-works/conclaves.md), [council](../how-it-works/council.md) and [integrations](../how-it-works/integrations.md).
- [Limits](limits.md) for every number named above.
