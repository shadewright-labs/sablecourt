# HTTP API reference

Every route that `sablecourtd` registers, grouped by the credential that reaches it.

Status: built.

The route tables are checked by `TestHTTPReferenceListsEveryRoute` in `cmd/sablecourtd`: it builds the daemon's handler with every optional part present, records each pattern registered on the router, and fails when a table here misses a route, lists one the daemon does not register, shows the wrong method, or lists a route twice. It also sends requests to every route without a credential, with the observer token and with the integration token, and fails when a route sits in the table of a credential class other than the one the handler applies. Every route in a table that needs a credential must refuse, with 401 or 403, a request with no credential and requests with a wrong operator token, a wrong task lease and a wrong token of any other shape. A route under [Public routes](#public-routes) must answer a request with no credential; `POST /login` may refuse it only for its same-origin rule. The two routes under [Inbound integrations](#inbound-integrations-parked) count as public for the daemon's checks, but the test requires their own check to refuse a request with no credential. The test clears the environment variables the handlers read, so the caller's shell does not change its result. `go test ./...` runs it. The method and path are the router pattern as written in the code, with these conventions:

- `ANY` marks a pattern registered without a method: the router passes every method to the handler. The purpose then says which methods the handler accepts (`Methods:`). The test finds them by sending `GET`, `HEAD`, `POST`, `PUT`, `PATCH`, `DELETE`, `OPTIONS` and the extension method `PROPFIND` with a valid task lease, and `any` means all eight; a method not listed is answered with 405. A method counts as accepted only when the handler really takes it: the Model Gate forwards it once to the upstream, the MCP route answers an `initialize` request with 200, and the action route reaches its catalog lookup. Any other answer fails the test. The probe cannot show that an action runs, because that needs an approval and makes a change.
- A path ending in `/` matches everything below it. `{$}` matches the path exactly, so `GET /{$}` is the root page only.
- A body or reply written as `{"a","b"}` lists the field names of a JSON object. It is not a body to copy.
- A `GET` pattern also answers `HEAD`, as Go's router does.
- The Model Gate is one mount at `/gate/model/`; its rows list the provider paths it forwards, taken from the gate's own route table. Any other path below the mount is refused with 404.

Purposes are one line each. Status codes and error texts are not listed, because no test ties them to this page; the [how it works](../how-it-works/index.md) pages describe the behavior. Errors from the `/api/v1/` and `/gate/` routes are JSON of the form `{"error": "..."}`, apart from the plain-text `500 streaming unsupported` of the four server-sent stream routes, the plain-text replies of the login, logout and metrics routes, the JSON-RPC errors of `POST /a2a`, the plain-text replies of the MCP SDK on `/gate/mcp`, and Go's plain-text `405 Method Not Allowed` for a registered path called with another method. The dashboard pages and form posts reply in HTML or plain text, and a browser `GET` without credentials gets an HTML `401` that links to `/login`.

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
| Cross-origin | A write (any method but `GET`, `HEAD` and `OPTIONS`) that carries `Origin` or `Sec-Fetch-Site` must be same-origin. `/gate/`, `POST /api/v1/hooks/github` and `POST /a2a` skip this check. A write that authenticates with the session cookie must also be same-origin, whether or not it carries those headers. |
| Body size | A write outside `/gate/model/` and `POST /api/v1/voice/transcribe` is cut off at 2 MiB. The voice route has its own cap, and the Model Gate reads at most 16 MiB of a request body. The values are on the [limits](limits.md#http-server) page. |

## Public routes

Source: `cmd/sablecourtd/main.go`, `internal/ui/ui.go`.

| Route | Purpose |
| --- | --- |
| `GET /healthz` | Liveness. Returns `{"status":"ok"}`. |
| `GET /login` | HTML sign-in form. |
| `POST /login` | Exchange the operator token, sent as the form field `token`, for a session cookie. |
| `GET /.well-known/agent-card.json` | Agent card with one skill for each loaded workload outside the `sandbox` zone. |
| `GET /favicon.ico` | Dashboard icon. |
| `GET /apple-touch-icon.png` | Dashboard icon. |
| `GET /assets/` | Dashboard static files. |

## Operator routes

### Tasks

Source: `cmd/sablecourtd/main.go`.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/workloads` | List workload records. |
| `GET /api/v1/tasks` | List tasks. |
| `POST /api/v1/tasks` | Submit a task. Body `{"workload", "idempotencyKey", "inputJson"}`. |
| `GET /api/v1/tasks/{id}` | The task, its state events and its budget totals as `{"task","events","budget"}`. |
| `POST /api/v1/tasks/{id}/cancel` | Cancel a task. |
| `POST /api/v1/tasks/{id}/retry` | Submit a new task from the input of a task in a terminal state. A council task is refused. With `?handoff=true` (or `1`) it attaches a failure handoff. |
| `POST /api/v1/tasks/{id}/suspend` | Checkpoint a working task. |
| `POST /api/v1/tasks/{id}/resume` | Resume a suspended task. |
| `GET /api/v1/tasks/{id}/scry` | Server-sent stream of redacted task output (Scry). Observer credential accepted. |
| `POST /api/v1/tasks/{id}/takeover` | Take exclusive stdin control of a live task session that has a writable stdin; refused while another operator holds it. Body `{"operator"}`. |
| `POST /api/v1/tasks/{id}/stdin` | Write to the task's stdin while holding the takeover. Body `{"operator","data"}`. |
| `POST /api/v1/tasks/{id}/detach` | Release the takeover. Body `{"operator"}`. |
| `GET /api/v1/stream` | Server-sent `state` events with a fingerprint of tasks, pending approvals and the newest audit row, sent at once and every 3 seconds, for the dashboard. |
| `GET /api/v1/budget` | Token and micro-USD totals over all tasks. |

### Approvals and devices

Source: `cmd/sablecourtd/main.go`. Device pairing is disabled: both pairing routes refuse every call.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/approvals` | List approvals, newest first. `?status=` filters by state. |
| `POST /api/v1/approvals/{id}/approve` | Approve after the out-of-band confirmation. Body `{"expectedSha256","decidedBy"}`; `expectedSha256` is required and must equal the approval's payload digest: an empty or missing digest is refused with 409 (`expected payload digest is required`) before the approval is read, and the confirmation dialog on the daemon host shows the stored value and is checked against the same digest. A body with `deviceId` and `signature` takes the device-signature path. |
| `POST /api/v1/approvals/{id}/deny` | Deny. Body `{"decidedBy","reason"}`. |
| `POST /api/v1/devices/pair/start` | Disabled. |
| `POST /api/v1/devices/pair/complete` | Disabled. |
| `GET /api/v1/devices` | List paired devices. |
| `POST /api/v1/devices/{id}/revoke` | Revoke a device. |

### Audit, memory, manifests and providers

Source: `cmd/sablecourtd/main.go`.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/audit` | The newest 50 audit rows, newest first. |
| `GET /api/v1/audit/verify` | Walk the audit hash chain. 200 with `verifiedRows`, `legacyRows` (version 1 rows) and the chain `head` (`seq`, `id`, `hash`); 409 with the error when a row has no hash or does not match, or the chain does not reach the optional `anchor=SEQ:HASH`; 400 for a malformed anchor. |
| `GET /api/v1/audit/export` | The newest 10000 audit rows as `application/x-ndjson`, oldest first. |
| `GET /api/v1/memory` | List up to 100 Court Memory entries, never those in a `learning:` namespace. Query `namespace` and `q` (substring match). |
| `POST /api/v1/memory` | Set an entry; the value is redacted, and a `learning:` namespace is refused with 403. Body `{"namespace","key","value"}`. |
| `DELETE /api/v1/memory/{namespace}/{key}` | Delete an entry; a `learning:` namespace is refused with 403. |
| `GET /api/v1/schedules` | List declared Schedules. |
| `POST /api/v1/schedules/{name}/run` | Run a Schedule now. |
| `POST /api/v1/reload` | Reload the manifests. |
| `POST /api/v1/promote/{name}` | For a `sandbox` workload, run `git branch` to create the branch `sablecourt/promote-<name>` in the daemon's working directory. It does not change the zone; a workload already in the `runtime` zone gets 409. |
| `GET /api/v1/runtime/coding` | How coding tasks and council seats run: `{"mode","hostModeOptIn"}`, where `mode` is `container`, `host-unsafe` (host mode forced on with `--unsafe-host-harnesses`, outside invariants 2 and 3) or `unavailable`. `sablectl doctor` prints it. |
| `GET /api/v1/providers` | Provider connection status and cached models. |
| `POST /api/v1/providers/{id}/sync` | Refresh a provider's model list and return it. |
| `GET /api/v1/providers/tiers` | The `fast`, `coding` and `critic` model routes. |
| `POST /api/v1/providers/tiers` | Save model routes. Body is a list of `{"tier","provider","model"}`. |
| `GET /api/v1/learnings` | Up to 100 learnings from Court Memory that match `?repo=` and `?path=` (a comma-separated list). |
| `POST /api/v1/learnings` | Disabled: refuses every call ([ADR-0012](../adr/0012-learning-evidence-and-expiry.md)). |
| `POST /api/v1/notify/test` | Send a test alert, with a redacted title and text, to the configured chat webhook (secret `notify_webhook_url`, or `SABLECOURT_NOTIFY_WEBHOOK_URL`). With no webhook configured nothing is sent and the reply is still 200 `{"status":"sent"}`; a failed send is 502. |

### Metrics and sign-out

Source: `cmd/sablecourtd/main.go`, `cmd/sablecourtd/operator_auth.go`, `internal/telemetry/metrics.go`.

| Route | Purpose |
| --- | --- |
| `GET /metrics` | Prometheus text format, see [metrics](metrics.md). |
| `POST /logout` | End every dashboard session by replacing the session signing key, and clear the cookie. The request must be same-origin. |

### Voice

Source: `cmd/sablecourtd/main.go`, `internal/voice/voice.go`. The audio goes to the speech-to-text endpoint (default a local address, set with `SABLECOURT_STT_URL` or the secret `voice_stt_url`) and the text of `speak` goes to the text-to-speech endpoint (`SABLECOURT_TTS_URL` or `voice_tts_url`; none by default). Either can point off the machine.

| Route | Purpose |
| --- | --- |
| `GET /api/v1/voice/status` | The resolved speech settings. |
| `POST /api/v1/voice/transcribe` | Speech to text: raw audio or `multipart/form-data` with a `file` part. Returns the redacted transcript. |
| `POST /api/v1/voice/speak` | Text to speech: the text is redacted, cut to 4,000 bytes (after redaction, so a multi-byte character can be split) and sent to the configured text-to-speech endpoint, and the audio comes back. Body `{"text","voice"}`. |
| `POST /api/v1/conclaves/{id}/voice-decree` | Audio Decree to a Conclave: the audio is transcribed as for `transcribe` and the redacted text is posted as a Decree to the role in `to` (query or form field, `all` if absent). |

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
| `ref` | string | none | Branch, tag, `HEAD` or full commit ID, at most 256 bytes, not starting with `-`. Cannot be combined with `live: true`. |
| `live` | bool | `true` when neither `ref` nor `writer` is set | Read-only view of the working tree. `true` cannot be combined with `ref` or `writer`; `false` needs a `ref` or `writer`. |
| `writer` | string | none | `claude`, `codex` or `agy`. With no `ref`, the ref is `HEAD`. `sablectl council --writer <seat>` sets it. |
| `rounds` | integer | `2` | At least 1. |
| `starts` | integer | `6` | From 1 to 6. |
| `durationSeconds` | integer | `900` | At least 1. |
| `with` | list of strings | `claude`, `codex`, `agy` | Must list all three once each, in any order. |
| `sequential` | bool | `false` | Run the members of a round one after another. |

Header `Idempotency-Key`: at most 256 characters, no control characters, one header only, and no operator token or secret in it. A repeat with the same key and the same body returns the existing session; a different body is refused. With Docker configured, opening checks each harness's image, login volume, vendor tool and login; without Docker (the host mode, unsupported for 0.1.0) it needs the three harness executables on the daemon's path and a working Seatbelt sandbox instead. Opening is refused in Citadel mode.

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
| `POST /ui/conclaves/{id}/learning` | Form post: save a proposed learning (`status: "proposed"`, pending review) from a Conclave message under `proposed:learning:<scope>`. |
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

Source: `cmd/sablecourtd/main.go`, `internal/gate/model/gate.go`, `internal/gate/mcp/gate.go`. The lease must be active: known by its token hash, not expired and not revoked. The task must also be `working` or `input-required`; for the routes under `/gate/` the daemon refuses a lease of any other task with 401.

| Route | Purpose |
| --- | --- |
| `ANY /gate/model/openai/v1/chat/completions` | Model Gate to OpenAI. Methods: any. The method is forwarded unchanged, also to a fallback provider. |
| `ANY /gate/model/openai/v1/responses` | Model Gate to OpenAI. Methods: any. The method is forwarded unchanged, also to a fallback provider. |
| `ANY /gate/model/local/v1/chat/completions` | Model Gate to a local OpenAI-compatible endpoint. Methods: any. The method is forwarded unchanged, also to a fallback provider. |
| `ANY /gate/model/local/v1/responses` | Model Gate to a local OpenAI-compatible endpoint. Methods: any. The method is forwarded unchanged, also to a fallback provider. |
| `ANY /gate/model/anthropic/v1/messages` | Model Gate to Anthropic. Methods: any. The method is forwarded unchanged. There is no fallback provider. |
| `ANY /gate/model/gemini/v1beta/models/{model}:generateContent` | Model Gate to Gemini, any model name. Methods: any. The method is forwarded unchanged. There is no fallback provider. |
| `ANY /gate/model/gemini/v1beta/models/{model}:streamGenerateContent` | Model Gate to Gemini, any model name. Methods: any. The method is forwarded unchanged. There is no fallback provider. |
| `ANY /gate/mcp` | MCP over streamable HTTP, stateless. Methods: `POST`. Lists only the tools granted to the workload and registered in the gate. |
| `ANY /gate/actions/` | Typed action from the closed catalog, named by the path below the mount (for example `/gate/actions/git.open_pr`). Methods: `POST`. Body `{"approvalId","payload"}`. |
| `GET /gate/task/messages` | Messages of the lease's task. |
| `POST /gate/task/messages` | Add a redacted message to the lease's task, always stored with the role `agent` (a `role` of `system` is refused with 403). Body `{"role","content"}`; `content` is required. |
| `POST /gate/conclave/{id}/messages` | Post a proposal as the lease's seat. |
| `GET /gate/conclave/{id}/messages` | Read messages visible to the seat. Query `since`. |
| `POST /gate/conclave/{id}/relics` | Store a Relic as the seat. |
| `GET /gate/conclave/{id}/relics/{name}` | Read a Relic the seat may see. |

The MCP route is served by the MCP Go SDK (v1.8.0) in stateless mode. If the daemon runs with `MCPGODEBUG=allowsessionsinstateless=1` in its environment, the SDK also answers `DELETE` with an `Mcp-Session-Id` header with 204 and does nothing. The SDK reads the variable once at start, so when it is set the test runs itself again in a new process without it.

### Council gate allow-list

Source: `internal/gate/councilproxy/proxy.go`. A council container reaches the daemon only through its own gate, and the gate forwards only these requests: `GET`, `POST`, `DELETE /gate/mcp`; `GET`, `POST /gate/task/messages`; `POST /gate/actions/{action}`; `GET`, `POST /gate/conclave/{id}/messages`; `POST /gate/conclave/{id}/relics`; `GET /gate/conclave/{id}/relics/{name}`. It never forwards `/gate/model/`. It also opens `CONNECT` tunnels on port 443 to the provider hosts of its own harness.

## Inbound integrations (parked)

Source: `cmd/sablecourtd/main.go`, `internal/webhook/github.go`, `internal/a2a/handler.go`. These two routes skip the operator check and authenticate themselves.

| Route | Purpose |
| --- | --- |
| `POST /api/v1/hooks/github` | GitHub webhook, signed with HMAC SHA-256 in `X-Hub-Signature-256` using the `github_webhook_secret` secret. An `issues` event with action `opened` submits a task to the workload `triage-agent`. |
| `POST /a2a` | JSON-RPC 2.0 methods `tasks/send` and `tasks/get`, with a bearer token checked against the `a2a_bearer_token` secret. A request that presents the value of `observer_bearer_token` gets 403 only when that value equals `a2a_bearer_token`; otherwise it gets 401 like any wrong token. If the observer secret cannot be read, the route answers 503. |

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: passkey enrolment and per-session revocation. Neither exists on `main`. `POST /logout` ends all sessions at once by replacing the signing key; a single session cookie cannot be revoked on its own.

Decided in [ADR-0012](../adr/0012-learning-evidence-and-expiry.md): operator acceptance and scoped matching of learnings. `POST /api/v1/learnings` stays disabled until then.

## In depth

- [Identity and access](../how-it-works/identity-and-access.md), [model gate](../how-it-works/model-gate.md), [tools and actions](../how-it-works/tools-and-actions.md), [approvals](../how-it-works/approvals.md), [conclaves](../how-it-works/conclaves.md), [council](../how-it-works/council.md) and [integrations](../how-it-works/integrations.md).
- [Limits](limits.md) for every number named above.
