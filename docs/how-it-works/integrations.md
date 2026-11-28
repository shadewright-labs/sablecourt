# Integrations

Sablecourt has narrow, optional channels to two sibling products. Shadewright can hand over an incident and report a verification result. For Nightloom, Sablecourt offers read-only observer routes and marks the sessions it starts. [ADR-0007](../adr/0007-strict-boundary-with-nightloom.md) keeps the products apart: Sablecourt manages only workloads it launches and does not scan the host process table, and Nightloom does not launch agents.

Status: partly built. Incident intake, verification reports, the observer routes and the session marker are built. A status the caller can read (issue #53), a learnings read for analysts (issue #54, ADR-0012) and an incident council template (issue #55) are planned but not built.

## Shadewright: incident and verification

[![Incident calls: Shadewright posts an incident, the daemon opens a room and starts the always-on Vassals, Shadewright later posts a report, and status and learnings reads are marked not built yet.](../diagrams/incident-sequence.svg)](../diagrams/incident-sequence.svg "Open full size")

Both routes are self-authenticated: the operator check skips them and each handler checks the token itself.

| Route | Sent by | Effect |
| --- | --- | --- |
| `POST /api/v1/integrations/shadewright/incidents` | Shadewright, to hand over an incident | opens a Conclave for the incident |
| `POST /api/v1/integrations/shadewright/verifications` | Shadewright, to report a verification result | posts the result into that Conclave |

Sablecourt checks the token and not which Shadewright component sends the request. The credential is the secret `shadewright_bearer_token` (store it with `sablectl secret set shadewright_bearer_token`), sent as one `Authorization: Bearer` header. The token comparison takes constant time. A token that starts with `op_` or `sc_task_` is refused, and a request that carries the Shadewright token as a bearer token on any other route gets 401. The routes exist only when the daemon runs the Conclave broker. The body is one JSON object of at most 2 MiB.

### Incident intake

The request carries `incidentId` (letters, digits, `_` and `-`, up to 128 characters, starting with a letter or digit), `summary` (required), and optionally `conclaveTemplate`, `cluster`, `namespace`, `workload`, `evidence` (an object) and `logs`:

```json
{
  "incidentId": "inc-2026-0001",
  "conclaveTemplate": "incident-room",
  "cluster": "lab",
  "namespace": "shop",
  "workload": "checkout",
  "summary": "checkout crashes on start after the last release",
  "evidence": {"restarts": 14},
  "logs": "panic: nil map assignment"
}
```

The daemon then:

1. Derives the room ID from the incident ID, `ccl-shadewright-` plus a SHA-256 hex digest, so a repeated incident ID maps to the same room. The title is "Shadewright Incident" plus that ID. An ID already used by a room that is not an incident room gets 409.
2. Picks the template: `conclaveTemplate` from the request, or the daemon flag `--incident-conclave-template`. For a room that already exists, the template of that room. It must name a loaded `Conclave`; otherwise 400.
3. Creates the room in state `active` and writes the audit row `conclave.open`.
4. Stores the redacted evidence as the Relic `incident-<room id>.json`, authored as `integration:shadewright`.
5. Starts the `always-on` Vassals of the template with a fixed prompt that names the Relic. Caller text is not put in that prompt. A Conclave limit such as `wards.maxRuns` can stop a start, and the request then gets 409.
6. Posts a `report` message from `integration:shadewright` to `all` with the summary, wakes every `on-mention` Vassal of the template whatever the summary says, and writes the audit row `shadewright.incident_escalated`. A Conclave limit or pause can stop the wake without an error.

The reply is 201 with the room and the Relic for a new incident, and 200 for a repeat with the same evidence once the first request finished. A repeat with different evidence gets 409, and so does a repeat of an unfinished request for a room that is no longer active. After a restart halfway through, the audit rows tell the same request which steps are missing, and it completes them. A Vassal can see the Relic in its [context packet](context-and-learning.md) as untrusted data cut at 2048 bytes, if it is among the last four Relics of the room in name order and survives the 32 KiB trim.

A template that passes `sablectl validate`, which also checks Conclave references:

```yaml
apiVersion: sablecourt.io/v1alpha1
kind: Budget
metadata:
  name: incident-budget
spec:
  perTask:
    maxCostUSD: 2.50
  daily:
    maxCostUSD: 15.00
---
apiVersion: sablecourt.io/v1alpha1
kind: Workload
metadata:
  name: incident-analyst
  zone: runtime
spec:
  class: coding-agent
  runtime:
    type: worktree-cli
  coding:
    harness: claude
    repoPath: .
    sandbox: seatbelt
  grants:
    modelGate:
      allowed: true
      providers: [anthropic]
      budgetRef: incident-budget
---
apiVersion: sablecourt.io/v1alpha1
kind: Conclave
metadata:
  name: incident-room
spec:
  description: Room opened for each incident Shadewright hands over.
  budgetRef: incident-budget
  wards:
    maxRuns: 3
  vassals:
    - role: analyst
      workload: incident-analyst
      activation: always-on
      canMessage: [operator]
```

### Verification report

The request carries `conclaveId` and `status` (required), and optionally `incidentId`, `prNumber`, `summary` and `report` (an object). `status` is free text of at most 64 characters with no control characters. The daemon does not check its value. The room must be an incident room whose intake finished, or the answer is 404; a given `incidentId` must match the room, or 400; a room in `input-required` or closed answers 409.

The daemon stores the request fields (`conclaveId`, `incidentId`, `prNumber`, `status`, `summary`, `report`) as the Relic `verification-<digest>.json` (the digest is of the request, so a repeat reuses it), posts a `report` message from `integration:shadewright` to the `operator` role of the room, and writes `shadewright.verification_posted` with the status as its outcome. The message is stored for the room. No notification is sent and no Vassal is woken. The reply is 201 with `status: recorded`, also for a repeat.

## Nightloom: observer and session marker

Three read-only routes accept the operator credential or the secret `observer_bearer_token`. The observer token works on these routes with GET only. A token that starts with `op_` or `sc_task_` is not taken as an observer token.

| Route | Returns |
| --- | --- |
| `GET /api/v1/observer/snapshot` | `source`, `observedAt`, the active tasks (ID, workload, state, update time; states `submitted`, `working`, `input-required` and `suspended`), all Conclaves, the number of pending approvals and the budget totals |
| `GET /api/v1/observer/stream` | server-sent `observer` events, one at once and then every 3 seconds, each holding three counts `tasks:conclaves:approvals` (all tasks, all Conclaves, pending approvals) |
| `GET /api/v1/tasks/{id}/scry` | live redacted output of one task, writes an audit row `task.scry` (present when the attach hub runs) |

Sablecourt marks the workloads it starts. A coding task runs with `SABLECOURT_MANAGED=1`, `SABLECOURT_TASK_ID` and `SABLECOURT_WORKLOAD` in its environment. Outside a council checkout, its worktree also holds `.sablecourt-session.json` with `managed_by`, `task_id`, `workload`, `zone`, `started_at` and, for Conclave tasks, `conclave_id` and `role`. Git excludes the file and cleanup removes it. Docker and Kubernetes workloads get the same three environment variables.

## Not built yet

- A status route for the caller, `GET /api/v1/integrations/shadewright/incidents/{incidentId}`: state, round, a short redacted result and links to any pull request the council opened, with no message bodies.
- A learnings read for the integration token, by cluster, namespace, workload kind and symptom, returning accepted, unexpired learnings under a size cap, and verification posts that name the learnings used and record an outcome on each. ADR-0012 describes this read, and it depends on the learning record described in [context and learning](context-and-learning.md).
- An incident council template shipped as an example: a read-only analysis round by two participants, a cross-review round and a digest with the proposed cause. Today intake opens whatever template you name.
- Shadewright proposing learnings.

## In depth

- [ADR-0007](../adr/0007-strict-boundary-with-nightloom.md), [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md), [ADR-0012](../adr/0012-learning-evidence-and-expiry.md)
- [Identity and access](identity-and-access.md) for the credential classes, [Conclaves](conclaves.md) for rooms
- Code: `internal/integrations/shadewright.go`, `cmd/sablecourtd/operator_auth.go`, `cmd/sablecourtd/main.go`, `internal/runtime/coding/worktree.go`, `internal/runtime/coding/coding.go`
