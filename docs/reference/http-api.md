# HTTP API reference

Every route that `sablecourtd` registers, grouped by the credential that reaches it.

Status: built.

The route tables are checked by `TestHTTPReferenceListsEveryRoute` in `cmd/sablecourtd`: it builds the daemon's handler with every optional part present, records each pattern registered on the router, and fails when a table here misses a route, lists one the daemon does not register, shows the wrong method, or lists a route twice. `go test ./...` runs it. The method and path are the router pattern as written in the code, with these conventions:

- `ANY` marks a pattern registered without a method: the handler behind it sees every method.
- A path ending in `/` matches everything below it. `{$}` matches the path exactly, so `GET /{$}` is the root page only.
- A `GET` pattern also answers `HEAD`, as Go's router does.
- The Model Gate is one mount at `/gate/model/`; its rows list the provider paths it forwards, taken from the gate's own route table. Any other path below the mount is refused with 404.

Purposes are one line each. Status codes and error texts are not listed, because no test ties them to this page; the [how it works](../how-it-works/index.md) pages describe the behavior. Errors from the API are JSON of the form `{"error": "..."}`, apart from the plain-text replies of the login, logout and metrics routes and the JSON-RPC errors of `POST /a2a`.

## Credential classes

Source: `cmd/sablecourtd/operator_auth.go`, `internal/integrations/shadewright.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`.

| Class | Credential | Reaches |
| --- | --- | --- |
| Public | None. The handler may apply its own check. | `GET /login`, `POST /login`, `GET /healthz`, the agent card, the dashboard's static files, and the two routes under [Inbound integrations](#inbound-integrations-parked). |
| Operator | `Authorization: Bearer op_...` with the contents of the `operator.token` file, or the `sablecourt_session` cookie that `POST /login` sets. The cookie counts only when the request has no `Authorization` header. | Every route not listed under another class, including `GET /metrics` and `POST /logout`. |
| Observer | `Authorization: Bearer` with the `observer_bearer_token` secret. `GET` only. | The [observer routes](#observer-routes) and `GET /api/v1/tasks/{id}/scry`. The operator credential also works on these. |
| Integration | `Authorization: Bearer` with the `shadewright_bearer_token` secret. | The two [integration routes](#integration-routes) only. On any other route this token is refused. |
| Task lease | `Authorization: Bearer sc_task_...`, minted per task. The Model Gate also accepts the token in `x-api-key` or `x-goog-api-key`. | Every route under `/gate/`. |

## Request rules

Source: `cmd/sablecourtd/operator_auth.go`, `cmd/sablecourtd/host.go`. Each rule refuses a request before it reaches a route.

| Rule | What it checks |
| --- | --- |
| Host check | The `Host` header must be `localhost`, `127.0.0.1` or `[::1]` with the listen port, or an entry from `--allow-host`. `/gate/` paths are also accepted on the `--gate-host` authority. |
| Path form | A path that is not in canonical form (dot segments, doubled slashes) is refused. |
| Cross-origin | A write (any method but `GET`, `HEAD` and `OPTIONS`) that carries `Origin` or `Sec-Fetch-Site` must be same-origin. `/gate/`, `POST /api/v1/hooks/github` and `POST /a2a` skip this check. |
| Body size | Every write has a body cap; the values are on the [limits](limits.md#http-server) page. |

## Public routes

Source: `cmd/sablecourtd/main.go`, `internal/ui/ui.go`.

| Route | Purpose |
| --- | --- |
| `GET /healthz` | Liveness. Returns `{"status":"ok"}`. |
| `GET /login` | HTML sign-in form. |
| `POST /login` | Exchange the operator token, sent as the form field `token`, for a session cookie. |
| `GET /.well-known/agent-card.json` | Agent card that lists every workload outside the `sandbox` zone. Parked. |
| `GET /favicon.ico` | Dashboard icon. |
| `GET /apple-touch-icon.png` | Dashboard icon. |
| `GET /assets/` | Dashboard static files. |

## Operator routes

### Tasks

Source: `cmd/sablecourtd/main.go`. The takeover, stdin and detach routes are parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md).

| Route | Purpose |
| --- | --- |
| `GET /api/v1/workloads` | List workload records. |
| `GET /api/v1/tasks` | List tasks. |
| `POST /api/v1/tasks` | Submit a task. Body `{"workload", "idempotencyKey", "inputJson"}`. |
| `GET /api/v1/tasks/{id}` | The task, its state events and its budget totals as `{"task","events","budget"}`. |
| `POST /api/v1/tasks/{id}/cancel` | Cancel a task. |
| `POST /api/v1/tasks/{id}/retry` | Submit a new task from the input of an old one. With `?handoff=true` (or `1`) it attaches a failure handoff. |
| `POST /api/v1/tasks/{id}/suspend` | Checkpoint a working task. |
| `POST /api/v1/tasks/{id}/resume` | Resume a suspended task. |
| `GET /api/v1/tasks/{id}/scry` | Server-sent stream of redacted task output (Scry). Observer credential accepted. |
| `POST /api/v1/tasks/{id}/takeover` | Take exclusive stdin control of a live task. |
| `POST /api/v1/tasks/{id}/stdin` | Write to the task's stdin while holding the takeover. |
| `POST /api/v1/tasks/{id}/detach` | Release the takeover. |
| `GET /api/v1/stream` | Server-sent `state` events with a fingerprint of tasks, approvals and audit rows, for the dashboard. |
| `GET /api/v1/budget` | Token and micro-USD totals over all tasks. |

### Approvals and devices

Source: `cmd/sablecourtd/main.go`. Device pairing is disabled: both pairing routes refuse every call.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/approvals` | List approvals, newest first. `?status=` filters by state. |
| `POST /api/v1/approvals/{id}/approve` | Approve after the out-of-band confirmation. Body `{"expectedSha256","decidedBy"}`. |
| `POST /api/v1/approvals/{id}/deny` | Deny. Body `{"decidedBy","reason"}`. |
| `POST /api/v1/devices/pair/start` | Disabled. |
| `POST /api/v1/devices/pair/complete` | Disabled. |
| `GET /api/v1/devices` | List paired devices. |
| `POST /api/v1/devices/{id}/revoke` | Revoke a device. |

### Audit, memory, manifests and providers

Source: `cmd/sablecourtd/main.go`.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/audit` | The newest audit rows, newest first. |
| `GET /api/v1/audit/verify` | Walk the audit hash chain and report whether it is valid. |
| `GET /api/v1/audit/export` | The newest audit rows as `application/x-ndjson`, oldest first. |
| `GET /api/v1/memory` | List Court Memory entries. Query `namespace` and `q` (substring match). |
| `POST /api/v1/memory` | Set an entry. Body `{"namespace","key","value"}`. |
| `DELETE /api/v1/memory/{namespace}/{key}` | Delete an entry. |
| `GET /api/v1/schedules` | List declared Schedules. |
| `POST /api/v1/schedules/{name}/run` | Run a Schedule now. |
| `POST /api/v1/reload` | Reload the manifests. |
| `POST /api/v1/promote/{name}` | For a `sandbox` workload, create the branch `sablecourt/promote-<name>` in the daemon's working directory. |
| `GET /api/v1/providers` | Provider connection status and cached models. |
| `POST /api/v1/providers/{id}/sync` | Refresh a provider's model list and return it. |
| `GET /api/v1/providers/tiers` | The `fast`, `coding` and `critic` model routes. |
| `POST /api/v1/providers/tiers` | Save model routes. Body is a list of `{"tier","provider","model"}`. |
| `GET /api/v1/learnings` | Learnings from Court Memory that match `?repo=` and `?path=`. |
| `POST /api/v1/learnings` | Disabled: refuses every call ([ADR-0012](../adr/0012-learning-evidence-and-expiry.md)). |
| `POST /api/v1/notify/test` | Send a test chat alert. Parked. |

### Metrics and sign-out

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `internal/telemetry/metrics.go`.

| Route | Purpose |
| --- | --- |
| `GET /metrics` | Prometheus text format, see [metrics](metrics.md). |
| `POST /logout` | End every dashboard session by replacing the session signing key, and clear the cookie. The request must be same-origin. |

### Voice

Source: `cmd/sablecourtd/main.go`, `internal/voice/voice.go`. All voice routes are parked.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/voice/status` | The resolved speech settings. |
| `POST /api/v1/voice/transcribe` | Speech to text: raw audio or `multipart/form-data` with a `file` part. Returns the redacted transcript. |
| `POST /api/v1/voice/speak` | Text to speech. Body `{"text","voice"}`. |
| `POST /api/v1/conclaves/{id}/voice-decree` | Audio Decree to a Conclave, transcribed by the voice backend. |

### Councils and conclaves

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/councils.go`, `cmd/sablecourtd/councils_detail.go`. A Conclave is a multi-agent room; a council is a Conclave opened with a bounded question.

| Route | Purpose |
| --- | --- |
| `POST /api/v1/councils` | Open a council. See the body table below. |
| `GET /api/v1/conclaves` | List Conclave sessions. |
| `POST /api/v1/conclaves` | Open a Conclave from a template. Body `{"template","title","decree"}`. |
| `GET /api/v1/conclaves/{id}` | `{"conclave","messages","relics"}`. |
| `GET /api/v1/conclaves/{id}/stream` | Server-sent `state` events with a fingerprint of the Conclave's state, messages and Relics. |
| `POST /api/v1/conclaves/{id}/decree` | Operator Decree. Body `{"to","body"}`. |
| `POST /api/v1/conclaves/{id}/close` | Close. Body `{"state"}`, `completed` or `cancelled`. |
| `POST /api/v1/conclaves/{id}/relics` | Store a Relic (a named, redacted artifact). Body `{"name","contentType","body"}`. |
| `GET /api/v1/conclaves/{id}/relics/{name}` | Read a Relic. |

`POST /api/v1/councils` body, from `decodeCouncilOpen`. Unknown fields, `null` values and wrong types are refused.

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

Header `Idempotency-Key`: at most 256 characters, no control characters, one header only, and no operator token or secret in it. A repeat with the same key and the same body returns the existing session; a different body is refused. Opening also needs the three harness executables on the daemon's path and a working Seatbelt sandbox, and is refused in Citadel mode.

### Dashboard

Source: `internal/ui/ui.go`. The web dashboard uses the operator credential and the same-origin rule.

| Route | Purpose |
| --- | --- |
| `GET /{$}` | Overview page. |
| `GET /tasks/{id}` | Task page. |
| `GET /providers` | Providers page. |
| `GET /studio` | Studio page for new agents, workflows and Conclaves. |
| `GET /conclaves` | Conclaves page. |
| `GET /conclaves/{id}` | Conclave page. |
| `GET /schedules` | Schedules page. |
| `GET /audit` | Audit page. |
| `GET /memory` | Court Memory page. |
| `POST /ui/tasks` | Form post: submit a task. |
| `POST /ui/tasks/{id}/retry` | Form post: retry a task. |
| `POST /ui/tasks/{id}/cancel` | Form post: cancel a task. |
| `POST /ui/tasks/{id}/suspend` | Form post: suspend a task. |
| `POST /ui/tasks/{id}/resume` | Form post: resume a task. |
| `POST /ui/tasks/{id}/messages` | Form post: add a message to a task. |
| `POST /ui/approvals/{id}/approve` | Form post: approve. |
| `POST /ui/approvals/{id}/deny` | Form post: deny. |
| `POST /ui/conclaves` | Form post: open a Conclave. |
| `POST /ui/conclaves/{id}/relics` | Form post: upload a Relic. |
| `POST /ui/conclaves/{id}/learning` | Form post: save a learning from a Conclave. |
| `POST /ui/conclaves/{id}/decree` | Form post: send a Decree. |
| `POST /ui/conclaves/{id}/close` | Form post: close a Conclave. |
| `POST /ui/memory/set` | Form post: set a Court Memory entry. |
| `POST /ui/memory/delete` | Form post: delete a Court Memory entry. |
| `POST /ui/providers/tiers` | Form post: save model routes. |
| `POST /ui/providers/{id}/connect` | Form post: connect a provider. |
| `POST /ui/providers/{id}/disconnect` | Form post: disconnect a provider. |
| `POST /ui/providers/{id}/sync` | Form post: refresh a provider's models. |
| `POST /ui/providers/{id}/models` | Form post: save a provider's models. |
| `POST /ui/schedules/{name}/run` | Form post: run a Schedule. |
| `POST /ui/workflows/{name}/run` | Form post: run a Workflow. |
| `POST /ui/studio/agent` | Form post: create an agent. |
| `POST /ui/studio/workflow` | Form post: create a Workflow. |
| `POST /ui/studio/conclave` | Form post: create a Conclave template. |

## Observer routes

Source: `cmd/sablecourtd/main.go`.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/observer/snapshot` | `{"source","observedAt","activeTasks","conclaves","pendingApprovals","budget"}`. |
| `GET /api/v1/observer/stream` | Server-sent `observer` events with a count fingerprint. |

## Integration routes

Source: `internal/integrations/shadewright.go`. Both take a JSON object and record their result in the matching Conclave.

| Route | Purpose |
| --- | --- |
| `POST /api/v1/integrations/shadewright/incidents` | Open a Conclave for a Shadewright incident and store the redacted evidence as a Relic. Required fields `incidentId` and `summary`. |
| `POST /api/v1/integrations/shadewright/verifications` | Store a verification report as a Relic of a Conclave. Required fields `conclaveId` and `status`. |

## Task-lease routes

Source: `cmd/sablecourtd/main.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`. The lease must be active: known by its token hash, not expired and not revoked. For `/gate/mcp`, `/gate/actions/`, `/gate/task/` and `/gate/conclave/` the task must also be `working` or `input-required`.

| Route | Purpose |
| --- | --- |
| `ANY /gate/model/openai/v1/chat/completions` | Model Gate to OpenAI. The method is forwarded unchanged. |
| `ANY /gate/model/openai/v1/responses` | Model Gate to OpenAI. The method is forwarded unchanged. |
| `ANY /gate/model/local/v1/chat/completions` | Model Gate to a local OpenAI-compatible endpoint. The method is forwarded unchanged. |
| `ANY /gate/model/local/v1/responses` | Model Gate to a local OpenAI-compatible endpoint. The method is forwarded unchanged. |
| `ANY /gate/model/anthropic/v1/messages` | Model Gate to Anthropic. The method is forwarded unchanged. |
| `ANY /gate/model/gemini/v1beta/models/{model}:generateContent` | Model Gate to Gemini, any model name. The method is forwarded unchanged. |
| `ANY /gate/model/gemini/v1beta/models/{model}:streamGenerateContent` | Model Gate to Gemini, any model name. The method is forwarded unchanged. |
| `ANY /gate/mcp` | MCP over streamable HTTP, stateless. Lists only the tools granted to the workload and registered in the gate. |
| `ANY /gate/actions/` | Typed action from the closed catalog, named by the path below the mount (for example `/gate/actions/git.open_pr`). The handler accepts `POST` only. Body `{"approvalId","payload"}`. |
| `GET /gate/task/messages` | Messages of the lease's task. |
| `POST /gate/task/messages` | Add a message to the lease's task. Body `{"role","content"}`. |
| `POST /gate/conclave/{id}/messages` | Post a proposal as the lease's seat. |
| `GET /gate/conclave/{id}/messages` | Read messages visible to the seat. Query `since`. |
| `POST /gate/conclave/{id}/relics` | Store a Relic as the seat. |
| `GET /gate/conclave/{id}/relics/{name}` | Read a Relic the seat may see. |

### Council gate allow-list

Source: `internal/gate/councilproxy/proxy.go`. A council container reaches the daemon only through its own gate, and the gate forwards only these requests: `GET`, `POST`, `DELETE /gate/mcp`; `GET`, `POST /gate/task/messages`; `POST /gate/actions/{action}`; `GET`, `POST /gate/conclave/{id}/messages`; `POST /gate/conclave/{id}/relics`; `GET /gate/conclave/{id}/relics/{name}`. It never forwards `/gate/model/`. It also opens `CONNECT` tunnels on port 443 to the provider hosts of its own harness.

## Inbound integrations (parked)

Source: `cmd/sablecourtd/main.go`, `internal/webhook/github.go`, `internal/a2a/handler.go`. These two routes skip the operator check and authenticate themselves.

| Route | Purpose |
| --- | --- |
| `POST /api/v1/hooks/github` | GitHub webhook, signed with HMAC SHA-256 in `X-Hub-Signature-256` using the `github_webhook_secret` secret. An `issues` event with action `opened` submits a task to the workload `triage-agent`. |
| `POST /a2a` | JSON-RPC 2.0 methods `tasks/send` and `tasks/get`, with a bearer token checked against the `a2a_bearer_token` secret. |

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: passkey enrolment, per-session revocation and a single-use `consumed` state for approvals. None of them exists on `main`. `POST /logout` ends all sessions at once by replacing the signing key; a single session cookie cannot be revoked on its own.

Decided in [ADR-0012](../adr/0012-learning-evidence-and-expiry.md): operator acceptance and scoped matching of learnings. `POST /api/v1/learnings` stays disabled until then.

## In depth

- [Identity and access](../how-it-works/identity-and-access.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md), [approvals](../how-it-works/approvals.md), [conclaves](../how-it-works/conclaves.md), [council](../how-it-works/council.md) and [integrations](../how-it-works/integrations.md).
- [Limits](limits.md) for every number named above.
