# Coding court example

`coding-court.yaml` is a manifest bundle for any Go repository, with four inline role cards, one command and three workflows. `architect-question.md` is a separate council question text, not a card. The same set written for this repository is in [`sablecourt/`](sablecourt/README.md). Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/coding-court.yaml
```

## The cards

A card is the inline `spec.roleCard` string of a Workload, so only the operator's manifests supply it ([ADR-0010](../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27). Author cards are short, about 1 KiB. Verifier cards carry the full checklist, read the tests before the production code, and need file and line evidence for a request for changes. A card names no model, no tier and no verdict format, and it grants nothing. Access comes from the workload's grants and the daemon's checks.

| Card | Used as | Harness | Intended model family | What it is for |
| --- | --- | --- | --- | --- |
| `developer-go` | author step | `claude` | Anthropic | Go changes with the failing test first. Reusable in a fix round, see [fix rounds](#fix-rounds). |
| `developer-docs` | author step | `claude` | Anthropic | Documentation and decision record text. |
| `reviewer` | verifier step | `codex` | OpenAI | Reviews the diff. The card tells the reviewer to work read only: no edit of a tracked file, but a temporary proving test is allowed and must be deleted. |
| `security` | verifier step | `agy` | Google (Gemini model) | Review of invariant paths, stated as failed controls. The card tells the reviewer to work read only: no edit of a tracked file, but a temporary test outside tracked paths is allowed and must be deleted. |
| `architect` (not a card) | council question text | none | none | Prose in `architect-question.md` that you paste into a council question about design. It is not the `roleCard` of any Workload here. It asks for open decisions as a numbered list, each with a recommended answer and the main alternative. You answer the numbered list in a Decree before round two. |

The verdict contract belongs to the control plane, not to the cards or the workflow. In `finalVerifierVerdict` (`internal/controlplane/workflow_evidence.go`) the verifier's single `final-message` text artifact, at most 64 KiB, loses its trailing CR and LF characters, and its last line must then equal `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED` exactly. Any other result gives no verdict and halts the workflow with `verdict_missing`: no final message or more than one, an echo of the prompt, a different last line (lowercase, `VERDICT: PASSING`, text after the verdict, the verdict only inside a longer line), or a final message over the limit. Most of these cases are in `TestWorkflowRequiresFinalVerifierVerdict`, and the halt with `verdict_missing` is in `TestWorkflowCheckoutAndVerdict`. No card names the format.

## Why three families

The ADR-0010 amendment requires a verifier step to run a model family that no step in its chain ran. The example aims for that: the developers use the `claude` harness (Anthropic), the reviewer `codex` (OpenAI) and the security reviewer `agy` with a Gemini model (Google). The family comes from the model, not the harness: `agy` can run Claude models, so its step names a Gemini model.

The families above are the intended routing, not an enforced rule. The verifier steps name `provider` and `model` themselves, because the compiled default `critic` route (`internal/contextpack/tiers.go`) is `cli:claude`, the author's harness, and a provider or model on the step replaces the route of its tier ([context and learning](../../docs/how-it-works/context-and-learning.md)). The model names are examples, so replace them with ones your harness lists. Enforcement of families waits for [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206), so today nothing in the daemon checks them. Until then, check the resolved family of every step before each verifier, fix rounds included. In a closed-loop fix round the reconciler submits the step recorded as the verifier's author step again with that step's own tier, provider and model (`internal/controlplane/reconciler.go`). For the first verifier that is the author step. For a second verifier in a row, such as `security` after `review`, it is the first verifier's step, so this example's `coding-invariant` would rerun `review`.

## Fix rounds

No workflow here sets `maxRetries`. The `developer-go` card is written to be reused in a fix round: it treats each finding as a claim to reproduce. A fix round starts in one of two ways. By hand, `sablectl task retry <task-id> --handoff` submits a new task for the same workload with the old task's input and the failure handoff block ([retry with a handoff](../../docs/how-to/retry-with-handoff.md)). Or you add `maxRetries` to a workflow step: while the retry count is below it, a verifier that ends with `VERDICT: CHANGES_REQUESTED` makes the reconciler run the step before it again (the author step for the first verifier, see [why three families](#why-three-families) for the second) with the verifier's redacted findings as the handoff ([verifier steps](../../docs/how-it-works/context-and-learning.md#verifier-steps)).

## The architect

The architect is a council question, not an inline card. The text in `architect-question.md` is for you to paste above the question you give a council. It is not a Workload and no workflow step uses it, and it binds no harness or model. The council seats are the built-in workloads `claude`, `codex` and `agy`. The text asks for a numbered list of open decisions; as an operator step, answer them in a Decree before round two.

## The workflows

- `coding`: `developer-go` (tier `coding`), then the `invariant-paths` command step, then `reviewer` (tier `critic`). The command step is the invariant path guard: it compares every path changed since the run's first author base with the path list in its argv and halts the run before the review when one matches, with a failure reason that names `coding-invariant` ([command steps](../../docs/how-it-works/command-steps.md)). Replace the example list with your repository's invariant paths; an empty list checks nothing.
- `coding-invariant`: `developer-go`, `reviewer`, then `security` (tier `critic`). Use it for a change on an invariant path. It has no guard.
- `docs-change`: `developer-docs`, then `reviewer`.

## Before you run it

- The `coding` blocks set `sandbox: container`, the value `sablectl init` writes. The loader does not check the field, and a task that runs in a harness container ignores it ([manifest reference](../../docs/reference/manifests.md)). Container runs need a Docker engine on the machine that runs the daemon. Without one, a coding task is refused unless host mode is forced, and then only `seatbelt` on macOS is accepted. Both claims are documented in [harness containers](../../docs/how-it-works/harness-containers.md) and the [manifest reference](../../docs/reference/manifests.md).
- Each workload sets `modelGate.allowed: false` and declares no actions and no delegation, so the manifest grants none of them. That is what the manifest grants. Separately, a workflow step with `role: verifier` gets a pinned checkout of the author's commit that the daemon mounts read only, so a verifier cannot write into the checkout at all. The reviewer and security cards also tell the role to delete any temporary proving test; that is card text, and the daemon does not check it. Add what your court needs, and keep verifier workloads free of actions and delegation.
- A test, `internal/contextpack/examples_cards_test.go`, is a byte-budget check: each inline card must be under 6 KiB, the packet size less stated reserves for the pre-check, learnings and header, and each author card under 1.25 KiB. The architect text is held to the same 6 KiB as an editorial limit, since no workload injects it. The test does not assemble a packet.
- Edit the cards to fit your repository. A card change is a manifest change.
