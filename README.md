# Sablecourt

Sablecourt is an active orchestrator for heterogeneous workloads under Shadewright Labs. It coordinates long-lived AI agents, ephemeral sandboxes promoted through GitOps, local coding agents (`claude`, `codex`, `agy`), and deterministic non-LLM controllers behind shared capability gates, budgets, and audit logs.

## Capabilities

- Manifest-driven control plane (`sablecourt.io/v1alpha1`): `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, and `Conclave`.
- Three Doors capability gates:
  - Model Gate: HTTP/SSE proxy for OpenAI, Anthropic, Gemini, and local OpenAI-compatible endpoints with per-task and daily cost accounting and zero-LLM enforcement for deterministic controllers.
  - Data and Action Gate: MCP Streamable HTTP server with allowlisted tools, closed typed GitOps actions, deterministic policy (`allow`, `approve`, `deny`), circuit breakers, and linear-time boundary secret redaction.
  - Approval Queue: pinned SHA-256 payload digests with macOS out-of-band confirmation dialogs.
- Multi-agent `Conclave` rooms: brokered communication between `Vassals` (`always-on` or `on-mention` activation), shared redacted `Relics`, operator `Decree` live steering, and deterministic `Wards` (`maxMessages`, `maxPingPong`, and `budgetRef` ceilings).
- Live process attach: `Scry` (read-only redacted output streaming across `coding-agent`, Docker, and Kubernetes Pods) and `Takeover` (exclusive `stdin` control for local coding agents).
- Persistence and deployment: embedded SQLite (WAL) or Postgres (`--postgres-dsn`), Kustomize Kubernetes deployment (`deploy/k8s/overlays/tailscale` and `deploy/k8s/overlays/public` with `--operator-auth-required`), Prometheus `/metrics`, GitHub webhooks, A2A JSON-RPC broker, and outbound alerts for Slack, Discord, ntfy, or generic webhooks.

Architecture decisions live in [docs/adr/](docs/adr/README.md) and the v0.1 design is in [docs/design.md](docs/design.md).

## Quickstart

Run the full test suite with the race detector:

```bash
go test -race ./...
```

Build and install `sablecourtd` and `sablectl`:

```bash
make install
```

Start the local daemon with the example manifests:

```bash
sablecourtd --manifests=examples/local-demo.yaml
```

Open the Web UI at `http://127.0.0.1:7432`, or drive the daemon from `sablectl`:

```bash
sablectl workload list
sablectl conclave open feature-conclave --title="Fix retry timeout"
sablectl conclave list
```

For Kubernetes deployment with volume-mounted Secrets and Tailscale or public HTTPS ingress, see [deploy/k8s/README.md](deploy/k8s/README.md).
