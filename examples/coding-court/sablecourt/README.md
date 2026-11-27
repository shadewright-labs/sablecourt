# Coding court cards for the Sablecourt repository

`coding-court.yaml` here has the same five roles as the [generic set](../README.md), written for this repository. `architect-question.md` is the fifth role, council text rather than a card. Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/sablecourt/coding-court.yaml
```

Workload and workflow names carry a `sablecourt-` prefix, so they differ from the generic set. The test loads each file on its own.

## What differs from the generic set

| Part | Generic set | This set | Why |
| --- | --- | --- | --- |
| Author cards | About 1 KiB: one slice, failing test first, standard library first, run vet and tests, stop | About 1 KiB too, with the repository's guides, decision records, reference and SVG diagram rules | Both are short. The agent reads the invariants in AGENTS.md. The full checklists stay with the verifiers. |
| Reviewer card | Generic checklist: errors, bounds, concurrency, one decision path, tests | The six invariants checked against the diff, plus the defect list | The defects come from this codebase: lost writes, unbounded retries, skipped terminal steps, wider redaction matches. |
| Security card | Credentials, untrusted input, fail closed, bounds, state, dependencies | One control per invariant, a confirmed finding needs a failing test, a base-branch check | Invariant paths here are gates, leases, redaction, approvals, policy, isolation and the audit chain. |
| Docs author card | Source for each claim, universal words, docs checks | Adds generated reference blocks, SVG diagrams, accepted decision records stay as written | These are the repository's docs rules. |
| Architect (council question text, not a card) | Design options and open decisions | Adds the invariant each option strains and the decision record rule | ADRs here gate any widening. The council seats keep their own harness and model. |
| Prose | Plain sentences | No em or en dashes, no emails, personal paths, host names or credentials | Repository rule in AGENTS.md. |

Both sets share the structure: tests are read before production code, a request for changes needs file and line evidence, and a verifier ends with the verdict line the workflow defines. A card names no model, no tier and no verdict format, and grants nothing ([ADR-0010](../../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27).

The harness and family arrangement is the same as in the generic set: developers on one family, the reviewer on a second and security on a third. It is the intended routing, not an enforced one, until [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206): check the resolved family of every step before each verifier, fix rounds included. The final verdict line is parsed by the control plane (see the generic README). A fix round starts with `sablectl task retry <task-id> --handoff`. The coding blocks set `sandbox: container`, which the loader does not check and a harness container ignores. The reviewer and security cards tell the agent to work read only; the grants (`modelGate.allowed: false`, no actions, no delegation) are what the daemon enforces.
