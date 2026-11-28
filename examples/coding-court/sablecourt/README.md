# Coding court cards for the Sablecourt repository

`coding-court.yaml` here has the same four inline cards and three workflows as the [generic set](../README.md), written for this repository. `architect-question.md` is a council question text, not a card. Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/sablecourt/coding-court.yaml
```

Workload and workflow names carry a `sablecourt-` prefix, so they differ from the generic set. The test loads each file on its own.

## What differs from the generic set

| Part | Generic set | This set | Why |
| --- | --- | --- | --- |
| Author cards | About 1 KiB: one slice, failing test first, standard library first, run vet and tests, stop | About 1 KiB too, with the repository's guides, decision records, reference and SVG diagram rules | Both are short. The agent reads the invariants in AGENTS.md (the hard invariants, lines 5 to 14). The full checklists stay with the verifiers. |
| Reviewer card | Generic checklist: errors, bounds, concurrency, one decision path, tests | The six invariants checked against the diff, plus the defect list | The defect list names failure modes to review: lost writes, unbounded retries, skipped terminal steps, wider redaction matches. It does not claim each one happened in this codebase. |
| Security card | Credentials, untrusted input, fail closed, bounds, state, dependencies | One control per invariant, a confirmed finding needs a failing test, a base-branch check | Invariant paths here are gates, leases, redaction, approvals, policy, isolation and the audit chain. |
| Docs author card | Source for each claim, universal words, docs checks | Adds generated reference blocks, SVG diagrams, accepted decision records stay as written | These are the repository's docs rules. |
| Architect (council question text, not a card) | Design options and open decisions | Adds the invariant each option strains and the decision record rule | Weakening an invariant needs an accepted ADR first (AGENTS.md, line 7). The text binds no harness or model. |
| Prose | Plain sentences | No em or en dashes, no emails, personal paths, host names or credentials | Working rules in AGENTS.md, lines 20 and 21. |

Both sets share the structure: tests are read before production code, a request for changes needs file and line evidence, and a verifier ends with the verdict line that the control plane parses (see the [generic README](../README.md#the-cards)). A card names no model, no tier and no verdict format, and grants nothing ([ADR-0010](../../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27).

The harness and family arrangement is the same as in the generic set: developers on one family, the reviewer on a second and security on a third. These families are intended, and enforcement waits for [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206): check the resolved family of every step before each verifier, fix rounds included. Fix rounds are explained in the [generic README](../README.md#fix-rounds). The coding blocks set `sandbox: container`, which the loader does not check and a harness container ignores, and a coding task needs a container runtime unless host mode is forced (see [harness containers](../../../docs/how-it-works/harness-containers.md) and the [manifest reference](../../../docs/reference/manifests.md)).

Write access works as in the generic set. Both reviewer cards tell the role to edit no tracked file and allow a temporary proving test that is deleted afterwards. The generic reviewer card matches the Sablecourt one for that reason: a reviewer has to show that a test fails without the change. The manifest grants nothing beyond `modelGate.allowed: false`, no actions and no delegation. Separately, a workflow step with `role: verifier` gets a pinned checkout that the daemon mounts read only. The card text about proving tests and their deletion is not checked by the daemon.
