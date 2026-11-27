# Coding court example

`coding-court.yaml` is a manifest bundle with four role cards and three workflows. `architect-question.md` is the fifth card. Check the bundle with:

```sh
go run ./cmd/sablectl validate examples/coding-court/coding-court.yaml
```

## The cards

A card is the inline `spec.roleCard` string of a Workload, so only the operator's manifests supply it ([ADR-0010](../../docs/adr/0010-agentic-context-learnings-and-verification-pipeline.md), amendment of 2026-11-27). Each card says what the role does, what it must check and what it must never do. A card names no model and no tier, and it grants nothing. Access comes from the workload's grants and the daemon's checks.

| Card | Used as | Harness | Model family | What it is for |
| --- | --- | --- | --- | --- |
| `developer-go` | author step | `claude` | Anthropic | Go changes with the failing test first. Also the fix round: it gets the failure handoff. |
| `developer-docs` | author step | `claude` | Anthropic | Documentation and decision record text. |
| `reviewer` | verifier step | `codex` | OpenAI | Reads the diff, writes nothing, ends with a verdict line. |
| `security` | verifier step | `agy` | Google (Gemini model) | Read-only review of invariant paths, stated as failed controls. |
| `architect` | council question | none | none | Prose in `architect-question.md` to put into a council question about design. |

The verifier cards end with `VERDICT: PASS` or `VERDICT: CHANGES_REQUESTED`. The control plane accepts only those two lines, whatever a card says.

## Why three families

The ADR-0010 amendment requires a verifier step to run a model family that no step in its chain ran. The developers share one family. The reviewer and the security reviewer each take another, so every verifier differs from the author and from every fix round. The family comes from the model, not the harness: `agy` can run Claude models, so its step names a Gemini model.

The compiled default `critic` route resolves to the same harness as `coding`, which does not fit the `codex` and `agy` workloads. The verifier steps therefore name `provider` and `model` themselves. The model names are examples, so replace them with ones your harness lists. The family check itself arrives with [issue 206](https://github.com/shadewright-labs/sablecourt/issues/206).

## The architect

On `main`, council seats are the built-in workloads `claude`, `codex` and `agy`, and they take no card. The architect is therefore text for the operator to paste into a council question, not a Workload. Giving a council seat a card needs an amendment of ADR-0013.

## The workflows

- `go-change`: `developer-go` (tier `coding`), then `reviewer` (tier `critic`).
- `go-change-invariant-path`: the same, then `security` (tier `critic`). Use it for any change to redaction, approvals, the gate, harness isolation, the audit chain or policy.
- `docs-change`: `developer-docs`, then `reviewer`.

## Before you run it

- The `coding` blocks set no `sandbox`. Harness containers are the only coding mode for 0.1.0 ([ADR-0011](../../docs/adr/0011-sanctum-scope-trust-and-cli-harnesses.md), amendment of 2026-11-27), and they have no manifest field until [issue 181](https://github.com/shadewright-labs/sablecourt/issues/181) lands. The host sandbox values are unsupported.
- The grants hold no Model Gate access, no actions and no delegation. Add what your court needs, and keep verifier workloads free of actions and delegation.
- Edit the cards to fit your repository. A card change is a manifest change and changes the bundle digest.
