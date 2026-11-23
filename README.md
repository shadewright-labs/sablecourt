# Sablecourt

Sablecourt is an active orchestrator for heterogeneous workloads under Shadewright Labs. It coordinates long-lived AI agents, ephemeral agent sandboxes promoted through GitOps, coding agents, and deterministic non-LLM controllers behind shared capability gates, budgets, and audit logs.

## Status

v0.1 vertical slice complete (M1 through M4): manifest loader, SQLite WAL store, Model Gate (OpenAI, Anthropic, Gemini), MCP Data/Action Gate with boundary redaction, pinned-digest out-of-band approval queue, three workload adapters (`docker`, `coding`, `controller`), hub-and-spoke reconciler, and embedded Web UI. Architecture decisions are in [docs/adr/](docs/adr/README.md) and the design is in [docs/design.md](docs/design.md).

## Quickstart

Run tests with the race detector:

```bash
go test -race ./...
```

Build the control plane daemon and CLI:

```bash
go build ./cmd/sablecourtd
go build ./cmd/sablectl
```

Validate a directory or file of `sablecourt.io/v1alpha1` YAML manifests:

```bash
go run ./cmd/sablectl validate ./path/to/manifests.yaml
```
