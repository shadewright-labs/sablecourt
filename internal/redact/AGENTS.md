This file adds to the root AGENTS.md and does not repeat it.

# internal/redact

- Pattern rules here are best effort. They are not a security boundary (ADR-0015, invariant 3). Never describe or test them as a guarantee.
- The boundary is exact-match replacement of each secret the daemon holds in memory. That work is issue 180 and is not on main yet, so do not add code or docs that claim it.
- A rule change starts with generated corpus tests whose answers are known by construction: build the secret-shaped input and the expected output together, never copy a hit from the rule itself.
- A secret shape the rules knowingly miss is a row in the gap table of `known_gaps_test.go`, and `TestKnownGaps` pins today's result. A rule that starts covering a gap makes that test fail; move the row to the corpus (`TestCorpusMustHide` or `TestCorpusMustKeep`) in the same change. `TestKnownGapsDoc` fails when the generated table in `docs/how-it-works/tools-and-actions.md` differs from the gap table.
- Matching stays linear in input size. `TestRedactorLinearGrowth` in `growth_test.go` feeds each corpus class at two sizes 8x apart and fails if the time grows faster than a fixed ratio bound, not against an absolute wall-clock limit. Keep any new growth check relative in the same way.
- Use only RE2 patterns from the standard library, so no backtracking is possible.
- Tests are table-driven and pass under `go test -race ./internal/redact/...`.
- Do not describe the rules as a guarantee. The known-gap table in the docs is generated from test rows (`known_gaps_doc_test.go`), and docs should show examples from test rows, not extend a list of what the rules catch (ADR-0015).
