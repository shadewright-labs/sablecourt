# ADR-0006: Hub-and-spoke dispatch over A2A-compatible task envelopes

- Date: 2026-11-22
- Status: Accepted

## Context

Allowing arbitrary peer-to-peer calls between agents in v0.1 complicates budget accounting, creates hidden retry loops, and opens lateral prompt-injection paths. At the same time, inventing a proprietary internal RPC schema makes later interoperability with external A2A agents harder.

## Decision

Enforce a strict hub-and-spoke topology in v0.1: workloads communicate only with the Sablecourt Control Plane and its capability gates, with zero direct workload-to-workload network connectivity.

[![Workloads have no direct route to each other: every task, including one sent over A2A, enters through the control plane; the child-task proposal is shown as planned.](../diagrams/hub-and-spoke.svg)](../diagrams/hub-and-spoke.svg "Open full size")

Figure: the API has no workload-to-workload route and a child task goes through the control plane; the child task proposal, its policy check and the child start are dashed because no gate tool or route calls the proposal yet. Network-level isolation between containers is not drawn.

Represent every unit of work using an A2A-compatible task envelope:

- Capability metadata aligned with Agent Cards (workload ID, class, declared skills, input/output schemas, allowed gates).
- Explicit task states (`submitted`, `working`, `input-required`, `suspended`, `completed`, `failed`, `cancelled`).
- Typed artifacts, parent task reference, deterministic idempotency key, and explicit budget ceiling.

[![The task envelope fields grouped by who fills them: the callers of SubmitTask pass the target, input and key, the control plane sets the ID, state, parent link and artifacts, a workload proposing a child task with a sub-budget is drawn as planned, and the agent card comes from the manifest.](../diagrams/a2a-task-envelope.svg)](../diagrams/a2a-task-envelope.svg "Open full size")

Figure: the task envelope as `domain.Task` holds it and who fills each field. The callers of `SubmitTask` (the API, A2A, the dashboard, a webhook and the scheduler) pass the target workload, the input and an optional idempotency key, which becomes the task ID when empty; the control plane sets the task ID, the state, the parent link and the artifacts; the agent card comes from the manifest. A workload proposing a child task, with a delegate grant check and a sub-budget ceiling, is dashed: `ProposeChildTask` exists but has no caller, and it stores the sub-budget as given without checking it against the parent cap.

When one agent needs work from another in v0.1, it proposes a child task to the Control Plane; deterministic policy checks whether the caller is allowed to spawn that target workload and allocates a sub-budget from the parent cap before dispatching.

[![A parent workload would propose a child task to the control plane, which would check the delegate grant and create the child; the child start and its artifacts on the child task are built, how a result reaches the parent is not specified, and workload-facing task routes act only on the caller's own task.](../diagrams/a2a-delegation-sequence.svg)](../diagrams/a2a-delegation-sequence.svg "Open full size")

Figure: one delegation as a sequence. The start of a submitted task and the recording of its artifacts are built; the proposal, the policy check and the child task with its sub-budget are dashed because `ProposeChildTask` has no caller yet. The ADR does not specify how a child's result reaches the parent.

## Alternatives considered

- Direct peer-to-peer A2A mesh between agents from day one. Rejected because unconstrained agent graphs hide cost amplification, weaken audit attribution, and make circuit breaking much harder.
- Custom ad-hoc JSON messages per workload type. Rejected because it forces separate tracking and UI code for coding agents, AI actors, and deterministic controllers.

## Consequences

- Every delegation step is visible in one audit trail, bounded by parent budgets, and subject to policy and circuit breakers.
- Slightly higher latency for sub-task handoffs (one hop through the control plane), which is negligible compared to model inference or build times.

## Evolution options

- Brokered A2A gateway: expose registered Sablecourt workloads via standard `/.well-known/agent-card.json` and A2A HTTP/gRPC endpoints so external A2A clients or peer agents can negotiate tasks through Sablecourt's policy broker.
- Explicit workflow graphs (DAGs): layer declarative multi-step pipelines and fan-out/fan-in coordinators on top of the same task envelope without changing workload adapters.
- Cross-instance federation: let two Sablecourt deployments (for example local workstation and cloud cluster) exchange signed A2A tasks over mTLS with per-peer capability policies.
