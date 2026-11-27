# Coding court example

`coding-court.yaml` is a manifest bundle for any Go repository, with four role cards and three workflows. `architect-question.md` is the fifth role, council text rather than a card. The same roles written for this repository are in [`sablecourt/`](sablecourt/README.md). Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/coding-court.yaml
```

## The cards

A card is the inline `spec.roleCard` string of a Workload, so only the operator's manifests supply it ([ADR-0010](../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27). Author cards are short, about 1 KiB. Verifier cards carry the full checklist, read the tests before the production code, and need file and line evidence for a request for changes. A card names no model, no tier and no verdict format, and it grants nothing. Access comes from the workload's grants and the daemon's checks.

| Card | Used as | Harness | Model family | What it is for |
| --- | --- | --- | --- | --- |
| `developer-go` | author step | `claude` | Anthropic | Go changes with the failing test first. Reusable in a fix round, see [fix rounds](#fix-rounds). |
| `developer-docs` | author step | `claude` | Anthropic | Documentation and decision record text. |
| `reviewer` | verifier step | `codex` | OpenAI | Reviews the diff. The card says read only and allows a temporary proving test. |
| `security` | verifier step | `agy` | Google (Gemini model) | Review of invariant paths, stated as failed controls. The card says read only and allows a temporary proving test. |
| `architect` | council question | none, the council seats keep their own harness and model | none | Prose in `architect-question.md` that you paste into a council question about design, not an inline card of any Workload. It asks for open decisions as a numbered list, each with a recommended answer and the main alternative. You answer the numbered list in a Decree before round two. |

The control plane (`internal/controlplane/workflow_evidence.go`) accepts a verifier's result only when its final line is exactly `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED`. A missing or other final line halts the workflow with `verdict_missing`. No card names the format.

## Why three families

The ADR-0010 amendment requires a verifier step to run a model family that no step in its chain ran. The example aims for that: the developers use the `claude` harness (Anthropic), the reviewer `codex` (OpenAI) and the security reviewer `agy` with a Gemini model (Google). The family comes from the model, not the harness: `agy` can run Claude models, so its step names a Gemini model.

This is the intended routing, not an enforced rule. The verifier steps name `provider` and `model` themselves, because the compiled default `critic` route (`internal/contextpack/tiers.go`) is `cli:claude`, the author's harness. The model names are examples, so replace them with ones your harness lists. The daemon does not check families yet, which is [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206). Until then, check the resolved family of every step before each verifier, fix rounds included. A fix round runs the author step's own provider and model again.

## Fix rounds

No workflow here declares a retry. The `developer-go` card is written to be reused in a fix round: it treats each finding as a claim to reproduce. A fix round starts in one of two ways. You retry the task with the failure handoff, `sablectl task retry <task-id> --handoff` ([retry with a handoff](../../docs/how-to/retry-with-handoff.md)). Or you opt in to closed-loop remediation with `maxRetries` on the workflow, which these examples do not set.

## The architect

The architect is a council question, not an inline card. The text in `architect-question.md` is for you to paste above the question you give a council. It is not a Workload and no workflow step uses it. The council seats are the built-in workloads `claude`, `codex` and `agy`; they keep their own harness and model and take no card. Giving a council seat a card needs an amendment of ADR-0013. The architect asks for a numbered list of open decisions; you answer in a Decree before round two.

## The workflows

- `go-change`: `developer-go` (tier `coding`), then `reviewer` (tier `critic`).
- `go-change-security`: the same, then `security` (tier `critic`). Use it for a change to a sensitive path such as credentials, authorization or input handling.
- `docs-change`: `developer-docs`, then `reviewer`.

## Before you run it

- The `coding` blocks set `sandbox: container`, the value `sablectl init` writes. The loader does not check the field, and a task that runs in a harness container ignores it ([manifest reference](../../docs/reference/manifests.md)). Container runs need a Docker engine on the machine that runs the daemon ([harness containers](../../docs/how-it-works/harness-containers.md)). Without one, a coding task is refused unless host mode is forced, and then only `seatbelt` on macOS is accepted.
- Each workload sets `modelGate.allowed: false` and declares no actions and no delegation, so the loader grants none. The reviewer and security cards tell the agent to work read only and to delete any temporary test. The daemon enforces the grants, not the card text, and the examples do not claim filesystem enforcement. Add what your court needs, and keep verifier workloads free of actions and delegation.
- A test, `internal/contextpack/examples_cards_test.go`, is a byte budget: each card, and the architect text, must be under the packet size less stated reserves for the pre-check, learnings and header (6 KiB), and each author card under 1.25 KiB. It does not assemble a packet.
- Edit the cards to fit your repository. A card change is a manifest change.
