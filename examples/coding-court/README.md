# Coding court example

`coding-court.yaml` is a manifest bundle for any Go repository, with four role cards and three workflows. `architect-question.md` is the fifth card. The same roles written for this repository are in [`sablecourt/`](sablecourt/README.md). Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/coding-court.yaml
```

## The cards

A card is the inline `spec.roleCard` string of a Workload, so only the operator's manifests supply it ([ADR-0010](../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27). Author cards are short, about 1 KiB. Verifier cards carry the full checklist, read the tests before the production code, and need file and line evidence for a request for changes. A card names no model, no tier and no verdict format, and it grants nothing. Access comes from the workload's grants and the daemon's checks.

| Card | Used as | Harness | Model family | What it is for |
| --- | --- | --- | --- | --- |
| `developer-go` | author step | `claude` | Anthropic | Go changes with the failing test first. Also the fix round: it gets the failure handoff. |
| `developer-docs` | author step | `claude` | Anthropic | Documentation and decision record text. |
| `reviewer` | verifier step | `codex` | OpenAI | Reads the diff, writes nothing. |
| `security` | verifier step | `agy` | Google (Gemini model) | Read-only review of invariant paths, stated as failed controls. |
| `architect` | council question | none | none | Prose in `architect-question.md` for a council question about design. It asks for open decisions as a numbered list, each with a recommended answer and the main alternative. The operator answers in a Decree before round two. |

The verdict line is the one the workflow defines, and the control plane accepts only `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED` as the final line. No card sets its own.

## Why three families

The ADR-0010 amendment requires a verifier step to run a model family that no step in its chain ran. The developers share one family. The reviewer and the security reviewer each take another, so every verifier differs from the author and from every fix round. The family comes from the model, not the harness: `agy` can run Claude models, so its step names a Gemini model.

The compiled default `critic` route resolves to the same harness as `coding`, which does not fit the `codex` and `agy` workloads. The verifier steps therefore name `provider` and `model` themselves. The model names are examples, so replace them with ones your harness lists. The family check itself arrives with [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206).

## The architect

On `main`, council seats are the built-in workloads `claude`, `codex` and `agy`, and they take no card. The architect is therefore text for the operator to paste into a council question, not a Workload. Giving a council seat a card needs an amendment of ADR-0013.

## The workflows

- `go-change`: `developer-go` (tier `coding`), then `reviewer` (tier `critic`).
- `go-change-security`: the same, then `security` (tier `critic`). Use it for a change to a sensitive path such as credentials, authorization or input handling.
- `docs-change`: `developer-docs`, then `reviewer`.

## Before you run it

- The `coding` blocks set `sandbox: container`: each task runs in a harness container ([harness containers](../../docs/how-it-works/harness-containers.md)), which needs a Docker engine on the machine that runs the daemon. The host sandbox value `seatbelt` is only for forced host mode.
- The grants hold no Model Gate access, no actions and no delegation. Add what your court needs, and keep verifier workloads free of actions and delegation.
- A test, `internal/contextpack/examples_cards_test.go`, checks that each card fits the context packet with room left.
- Edit the cards to fit your repository. A card change is a manifest change and changes the bundle digest.
