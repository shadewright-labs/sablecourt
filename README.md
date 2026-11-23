# Sablecourt

Sablecourt is an active orchestrator for heterogeneous workloads under Shadewright Labs. It coordinates long-lived AI agents, ephemeral agent sandboxes promoted through GitOps, coding agents, and deterministic non-LLM controllers behind shared capability gates, budgets, and audit logs.

## Status

v0.1 M1 scaffold. Architecture decisions are recorded in [docs/adr/](docs/adr/README.md) and the v0.1 implementation design is in [docs/design.md](docs/design.md).

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
