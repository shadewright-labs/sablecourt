# Sablecourt

Sablecourt is an active orchestrator for heterogeneous workloads under Shadewright Labs. It coordinates long-lived AI agents, ephemeral sandboxes promoted through GitOps, local coding agents (`claude`, `codex`, `agy`), and deterministic non-LLM controllers behind shared capability gates, budgets, and audit logs.

## Capabilities

- Manifest-driven control plane (`sablecourt.io/v1alpha1`): `Workload`, `Policy`, `Budget`, `Workflow`, `Schedule`, and `Conclave`.
- Three Doors capability gates:
  - Model Gate: HTTP/SSE proxy for OpenAI, Anthropic, Gemini, and local OpenAI-compatible endpoints with per-task and daily cost accounting and zero-LLM enforcement for deterministic controllers.
  - Data and Action Gate: MCP Streamable HTTP server with allowlisted tools, closed typed GitOps actions, deterministic policy (`allow`, `approve`, `deny`), circuit breakers, and linear-time boundary secret redaction.
  - Approval Queue: pinned SHA-256 payload digests with macOS out-of-band confirmation dialogs.
- Multi-agent `Conclave` rooms: `always-on` Vassals launch once when a room opens, and `on-mention` Vassals launch when addressed. Rooms share redacted `Relics`, operator `Decrees`, and `Wards` with `maxMessages` and `maxPingPong` limits. The aggregate budget reference is `spec.budgetRef`.
- Live process attach: `Scry` (read-only redacted output streaming across `coding-agent`, Docker, and Kubernetes Pods) and `Takeover` (exclusive `stdin` control for local coding agents).
- Persistence and deployment: embedded SQLite (WAL) or Postgres (`--postgres-dsn`), Kustomize Kubernetes deployment (`deploy/k8s/overlays/tailscale` and `deploy/k8s/overlays/public` with `--operator-auth-required`), Prometheus `/metrics`, GitHub webhooks, A2A JSON-RPC broker, and outbound alerts for Slack, Discord, ntfy, or generic webhooks.

Architecture decisions live in [docs/adr/](docs/adr/README.md). Technical documentation:

- [docs/architecture.md](docs/architecture.md) - system topology, package map, Sanctum/Citadel modes, and request lifecycle
- [docs/reference.md](docs/reference.md) - manifest kinds (`sablecourt.io/v1alpha1`), HTTP/SSE/MCP APIs, and `sablectl` CLI commands
- [docs/integrations.md](docs/integrations.md) - upstream MCP server bridge, Shadewright Contract 3 escalation API, Kubernetes Deployments, Prometheus `/metrics`, Court Memory UI, and task retry
- [docs/design.md](docs/design.md) - original v0.1 design baseline

## State of the areas

| Area | State | Current behavior |
| --- | --- | --- |
| Court Memory search | Partly | Persistent entries use SQL `LIKE`/`ILIKE` search. FTS5 is not implemented. |
| Operator Relic upload | Partly | The UI route exists, but the Conclave page has no upload form. Use `sablectl conclave relic` or the API. |
| Conclave Vassals | Works | `always-on` Vassals launch once when a Conclave opens; `on-mention` Vassals launch when addressed. |
| Conclave voice readback | Partly | The auto-speak checkbox reads the last displayed message when the page loads or reloads after a room update. |
| Scoped learnings | Partly | Matching learnings are added to context packets for CLI coding tasks. Other task types do not receive them automatically. |
| Audit chain deletion detection | Parked | Verification checks hashes and links among rows it reads. It skips legacy rows without hashes and cannot detect deletion of the final row. See the tracking [epic #1](https://github.com/shadewright-labs/sablecourt/issues/1). |

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
