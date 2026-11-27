This file adds to the root AGENTS.md and does not repeat it.

# internal/redact

- Pattern rules here are best effort. They are not a security boundary (ADR-0015, invariant 3). Never describe or test them as a guarantee.
- The boundary is exact-match replacement of each secret the daemon holds in memory. That work is issue 180 and is not on main yet, so do not add code or docs that claim it.
- A rule change starts with generated corpus tests whose answers are known by construction: build the secret-shaped input and the expected output together, never copy a hit from the rule itself.
- A secret shape the rules knowingly miss is listed as a known gap with a test that pins today's result (issue 126). A rule that starts covering a gap moves that row to the corpus in the same change.
- Matching stays linear in input size. Prove it with growth tests that scale the input and compare work, never with a wall-clock limit.
- Use only RE2 patterns from the standard library, so no backtracking is possible.
- Tests are table-driven and pass under `go test -race ./internal/redact/...`.
- Docs describe rules through examples generated from test rows, never through a list of what they catch (ADR-0015).
