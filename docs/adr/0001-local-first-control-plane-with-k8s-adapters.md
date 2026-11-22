# ADR-0001: Local-first control plane with Kubernetes-ready adapters

- Date: 2026-11-22
- Status: Accepted

## Context

Sablecourt must orchestrate heterogeneous agents behind strict policy, budget, and audit boundaries. Starting directly on a full Kubernetes stack (Talos, CRDs, Valkey, S3, Trino, kcp) slows down early iteration on the core state machine, redaction rules, and capability gates, while locking the core to local macOS assumptions would block future cluster deployments.

## Decision

Build v0.1 as a single local-first Go control plane daemon backed by SQLite in WAL mode, running on macOS or Linux. Isolate every infrastructure concern (state store, workload runtime, secret provider, network transport) behind narrow Go interfaces so the core domain logic never imports container runtime or cluster SDKs directly.

In v0.1, declared intent (workload manifests, capability policies, budget rules) is loaded from Git-tracked files on disk, and runtime operational state (tasks, leases, budget counters, approvals, audit log) is stored in SQLite.

## Alternatives considered

- Kubernetes-first deployment on a local `kind` cluster with CRDs and `kubernetes-sigs/agent-sandbox`. Rejected for v0.1 because cluster plumbing, RBAC generation, and controller scaffolding would dominate the first weeks before the task protocol and gate contracts are proven.

## Consequences

- Fast local test loop (`go test -race ./...` with zero external cluster required) and trivial single-machine installation on a workstation or LAN host.
- Single-node availability only in v0.1; horizontal scaling and multi-node scheduling wait for later adapters.
- Discipline is required at every PR so host-specific paths or SQLite lock assumptions do not leak past the store and runtime interfaces.

## Evolution options

- LAN worker expansion: add an SSH or mTLS remote worker daemon so a Mac control plane can dispatch isolated workloads onto a Linux or WSL build host.
- Kubernetes operator mode: swap the SQLite store for Postgres or CRDs, bind declared manifests to Flux or Argo CD GitOps reconciliation, and run workloads through a Kubernetes runtime adapter (`kubernetes-sigs/agent-sandbox`, Jobs, or `agent-substrate`).
- Multi-tenant control plane: move from single-owner local identity to SPIFFE/SPIRE or cloud workload identity federation across multiple namespaces or clusters.
