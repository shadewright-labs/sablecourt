# HTTP API reference

Every route that `sablecourtd` serves, grouped by the credential that reaches it. Errors are JSON of the form `{"error": "..."}`.

Status: built.

## Credential classes

Source: `cmd/sablecourtd/operator_auth.go`, `internal/integrations/shadewright.go`, `internal/gate/model/gate.go`.

| Class | Credential | Reaches |
| --- | --- | --- |
| Public | None. The handler may apply its own check. | `/login`, `/healthz`, the agent card, static assets, and the two inbound routes in [Inbound integrations](#inbound-integrations-parked). |
| Operator | `Authorization: Bearer op_...` (the contents of the `operator.token` file) or the `sablecourt_session` cookie that `POST /login` sets. | Every route not listed under another class. |
| Observer | `Authorization: Bearer` with the `observer_bearer_token` secret. `GET` only. | `GET /api/v1/observer/snapshot`, `GET /api/v1/observer/stream`, `GET /api/v1/tasks/{id}/scry`. The operator credential also works on these. |
| Integration | `Authorization: Bearer` with the `shadewright_bearer_token` secret. | The two `/api/v1/integrations/shadewright/` routes only. On any other route this token is refused with 401. |
| Task lease | `Authorization: Bearer sc_task_...`, minted per task and valid for one hour. The Model Gate also accepts `x-api-key` and `x-goog-api-key`. | Every route under `/gate/`. |

## Request rules

Source: `cmd/sablecourtd/operator_auth.go`, `cmd/sablecourtd/host.go`, `cmd/sablecourtd/main.go`.

| Rule | Value |
| --- | --- |
| Host check | The `Host` header must be `localhost`, `127.0.0.1` or `[::1]` with the listen port, or an entry from `--allow-host`. Anything else is 403 `request host is not allowed`. Over the Unix socket the host is `unix`. |
| Gate host | `/gate/` paths are also accepted on the `--gate-host` authority (default `host.docker.internal:<port>` when listening on loopback). |
| Path form | A path that is not in canonical form (dot segments, doubled slashes) is 401 `canonical request path required`. |
| Cross-origin | A write that carries `Origin` or `Sec-Fetch-Site` must be same-origin, or it is 403 `cross-origin request blocked`. A write that uses only the session cookie must be same-origin. Bearer calls without those headers pass. |
| Body size | 2 MiB on every write, except `/gate/model/` (16 MiB read), `POST /api/v1/voice/transcribe` (24 MiB) and `POST /api/v1/councils` (1 MiB). |
| Response headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: no-referrer` and a `Content-Security-Policy` that allows only same-origin resources. |
| Session cookie | `sablecourt_session`: `Secure`, `HttpOnly`, `SameSite=Strict`, 30 minutes, stateless (signed with a key held in memory). `POST /login` accepts one form field `token`, at most 1 KiB, and limits failures to 10 per minute per client address (429 with `Retry-After`). |

## Public routes

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `internal/ui/ui.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /healthz` | Liveness. Returns 200 `{"status":"ok"}`. | built |
| `GET /login` | HTML sign-in form. | built |
| `POST /login` | Exchange the operator token for a session cookie. 303 to `/` on success, 401 on a wrong token, 403 if not same-origin, 415 if not a form. | built |
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
| `POST /api/v1/tasks/{id}/cancel` | Cancel a task. 200, or 409 if it cannot be cancelled. | built |
| `POST /api/v1/tasks/{id}/retry` | Submit a new task from the input of an old one. With `?handoff=true` (or `1`) it attaches a failure handoff. 201 with the new task, 404 if unknown, 400 otherwise. | built |
| `POST /api/v1/tasks/{id}/suspend` | Checkpoint a working task. 200 or 409. | built |
| `POST /api/v1/tasks/{id}/resume` | Resume a suspended task. 200 or 409. | built |
| `GET /api/v1/tasks/{id}/scry` | Server-sent stream of redacted task output (Scry): an `history` event, then `chunk` events, then `closed`. 404 if the task has no live session. Observer credential accepted. | built |
| `POST /api/v1/tasks/{id}/takeover` | Take exclusive stdin control. Body `{"operator"}`. 409 if held, 404 if no session. | parked |
| `POST /api/v1/tasks/{id}/stdin` | Write to the task's stdin. Body `{"operator","data"}`. | parked |
| `POST /api/v1/tasks/{id}/detach` | Release the takeover. Body `{"operator"}`. | parked |
| `GET /api/v1/stream` | Server-sent `state` event every 3 seconds with a fingerprint of task, approval and audit counts. | built |
| `GET /api/v1/budget` | Token and micro-USD totals over all tasks. | built |

### Approvals and devices

Source: `cmd/sablecourtd/main.go`.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `GET /api/v1/approvals` | List approvals. `?status=pending`, `approved`, `denied` or `expired` filters; no filter lists all. | built |
| `POST /api/v1/approvals/{id}/approve` | Approve after the out-of-band dialog. Body `{"expectedSha256","decidedBy"}`; `expectedSha256` is optional today. With `deviceId` and `signature` it takes the device path instead. 200, or 409 on any refusal. | built |
| `POST /api/v1/approvals/{id}/deny` | Deny. Body `{"decidedBy","reason"}`. 200 or 409. | built |
| `POST /api/v1/devices/pair/start` | Always 503 `device enrollment requires independent confirmation`. | disabled |
| `POST /api/v1/devices/pair/complete` | Always 503, same message. | disabled |
| `GET /api/v1/devices` | List paired devices. | built |
| `POST /api/v1/devices/{id}/revoke` | Revoke a device. 404 if unknown. | built |

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
| `POST /api/v1/schedules/{name}/run` | Run a Schedule now. 200, 404 if unknown, 409 if the run is refused. | built |
| `POST /api/v1/reload` | Reload the manifests. 200 `{"status":"reloaded"}`, 400 with the loader error. | built |
| `POST /api/v1/promote/{name}` | For a `sandbox` workload, create the branch `sablecourt/promote-<name>` in the daemon's working directory. 404 if the workload is unknown, 409 if it is not in the sandbox zone or the branch exists. | built |
| `GET /api/v1/providers` | Provider connection status and cached models. | built |
| `POST /api/v1/providers/{id}/sync` | Refresh a provider's model list. 502 on upstream failure. | built |
| `GET /api/v1/providers/tiers` | The `fast`, `coding` and `critic` model routes. | built |
| `POST /api/v1/providers/tiers` | Save routes. Body is a list of `{"tier","provider","model"}`. | built |
| `GET /api/v1/learnings` | Learnings from Court Memory that match `?repo=` and `?path=` (comma-separated), at most 100. | built |
| `POST /api/v1/learnings` | Always 410 `legacy learning publication is disabled`. | disabled |
| `POST /api/v1/notify/test` | Send a test chat alert. 502 if the webhook is unreachable. | parked |

### Councils and conclaves

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/councils.go`, `cmd/sablecourtd/councils_detail.go`. A Conclave is a multi-agent room; a council is a Conclave opened with a bounded question.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `POST /api/v1/councils` | Open a council. See the body table below. 201 with the session, or 200 with the existing session when `Idempotency-Key` repeats an identical request. | built |
| `GET /api/v1/conclaves` | List Conclave sessions. | built |
| `POST /api/v1/conclaves` | Open a Conclave from a template. Body `{"template","title","decree"}`. 201, 400 on refusal. | built |
| `GET /api/v1/conclaves/{id}` | `{"conclave","messages","relics"}`. For a council, each answer message carries its `round`. 404 if unknown. | built |
| `GET /api/v1/conclaves/{id}/stream` | Server-sent `state` event every 2 seconds with a fingerprint of state, message and Relic counts. | built |
| `POST /api/v1/conclaves/{id}/decree` | Operator Decree. Body `{"to","body"}`. 201, 409 if refused. | built |
| `POST /api/v1/conclaves/{id}/voice-decree` | Audio Decree, transcribed by the voice backend. 2 MiB cap. | parked |
| `POST /api/v1/conclaves/{id}/close` | Close. Body `{"state"}`, `completed` or `cancelled`; anything else means `completed`. 200 or 409. | built |
| `POST /api/v1/conclaves/{id}/relics` | Store a Relic (a named, redacted artifact). Body `{"name","contentType","body"}`; `contentType` defaults to `text/plain`. 201. | built |
| `GET /api/v1/conclaves/{id}/relics/{name}` | Read a Relic. 404 if unknown. | built |

`POST /api/v1/councils` body, from `decodeCouncilOpen`. Unknown fields, `null` values and wrong types are all `invalid council JSON object or field`.

| Field | Type | Default | Rule |
| --- | --- | --- | --- |
| `question` | string | none | Required. At most 30 KiB (31 and 744 bytes), no NUL, and the packed opening must fit 59 KiB encoded. |
| `decree` | string | none | If present, must equal `question` after trimming. |
| `title` | string | `Council` | At most 512 bytes. |
| `repo` | string | none | Required absolute path of a primary Git checkout owned by the daemon user, outside the state and credential directories. |
| `ref` | string | none | Branch, tag, `HEAD` or full commit ID, at most 256 bytes. Excludes `live`. |
| `live` | bool | `true` when neither `ref` nor `writer` is set | Read-only view of the working tree. Excludes `ref` and `writer`. |
| `writer` | string | none | `claude`, `codex` or `agy`. With no `ref`, the ref is `HEAD`. Reachable through the API only, `sablectl council` has no flag for it. |
| `rounds` | integer | `2` | At least 1. |
| `starts` | integer | `6` | From 1 to 6. |
| `durationSeconds` | integer | `900` | At least 1. |
| `with` | list of strings | `claude`, `codex`, `agy` | Must list all three once each, in any order. |
| `sequential` | bool | `false` | Run the members of a round one after another. |

Header `Idempotency-Key`: at most 256 characters, no control characters, one header only. A repeat with a different body is 409. Opening also needs the three harness executables on the daemon's path and a working Seatbelt sandbox (503 otherwise), and is 503 `council opening requires Sanctum` in Citadel mode.

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

Source: `cmd/sablecourtd/main.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`. The lease must be active: known by its token hash, not expired and not revoked.

| Method and path | Purpose | Status |
| --- | --- | --- |
| `POST /gate/model/openai/v1/chat/completions`, `POST /gate/model/openai/v1/responses`, `POST /gate/model/local/v1/chat/completions`, `POST /gate/model/local/v1/responses` | Model Gate to an OpenAI-compatible endpoint. | built |
| `POST /gate/model/anthropic/v1/messages` | Model Gate to Anthropic. | built |
| `POST /gate/model/gemini/v1beta/models/{model}:generateContent`, `...:streamGenerateContent` | Model Gate to Gemini. | built |
| `GET`, `POST`, `DELETE /gate/mcp` | MCP over streamable HTTP, stateless. Lists only the tools granted to the workload. | built |
| `POST /gate/actions/{action}` | Typed action from the closed catalog (`git.create_branch`, `git.open_pr`, `sandbox.propose_promotion`). Body `{"approvalId","payload"}`. 403 on deny, 202 with an approval ID when approval is needed, 404 for an unknown action. | built |
| `GET /gate/task/messages` | Messages of the lease's task. | built |
| `POST /gate/task/messages` | Add a message. Body `{"role","content"}`; the role `system` is 403, the stored role is `agent`. | built |
| `POST /gate/conclave/{id}/messages` | Post a proposal as the lease's seat. 403 if not allowed. | built |
| `GET /gate/conclave/{id}/messages` | Read messages visible to the seat. Query `since` (non-negative integer). | built |
| `POST /gate/conclave/{id}/relics` | Store a Relic as the seat. | built |
| `GET /gate/conclave/{id}/relics/{name}` | Read a Relic the seat may see. | built |

Any other path under `/gate/model/` is 404 `unknown model gate route`. A provider outside the workload's `providers` list is 403. Exhausted budget is 429 `budget exceeded: <reason>`. A 429, 502, 503 or 504 from `openai` or `local` falls back to the other if the workload grants it, and the response carries `X-Sablecourt-Fallback-Provider`.

### Council gate allow-list

Source: `internal/gate/councilproxy/proxy.go`. A council container reaches the daemon only through its own gate, and the gate forwards only these requests: `GET`, `POST`, `DELETE /gate/mcp`; `GET`, `POST /gate/task/messages`; `POST /gate/actions/{action}`; `GET`, `POST /gate/conclave/{id}/messages`; `POST /gate/conclave/{id}/relics`; `GET /gate/conclave/{id}/relics/{name}`. It never forwards `/gate/model/`.

## Inbound integrations (parked)

Source: `cmd/sablecourtd/main.go`, `internal/webhook/github.go`, `internal/a2a/handler.go`. These two routes skip the operator check and authenticate themselves.

| Method and path | Credential | Purpose | Status |
| --- | --- | --- | --- |
| `POST /api/v1/hooks/github` | HMAC SHA-256 in `X-Hub-Signature-256` with the `github_webhook_secret` secret | For an `issues` event with action `opened`, submit a task to the workload `triage-agent`. Other events return 200 `ignored`. 202 on dispatch. | parked |
| `POST /a2a` | Bearer `a2a_bearer_token` secret | JSON-RPC methods `tasks/send` (needs `workload`) and `tasks/get` (needs `id`). | parked |

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: `POST /logout` and server-side session revocation, passkey enrolment, and a single-use `consumed` state for approvals. None of them exists on `main`; the session cookie cannot be revoked before it expires.

Decided in [ADR-0012](../adr/0012-learning-evidence-and-expiry.md): operator acceptance and scoped matching of learnings. `POST /api/v1/learnings` stays disabled until then.

## In depth

- [Identity and access](../how-it-works/identity-and-access.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md), [approvals](../how-it-works/approvals.md), [conclaves](../how-it-works/conclaves.md), [council](../how-it-works/council.md) and [integrations](../how-it-works/integrations.md).
- [Limits](limits.md) for every number named above.
