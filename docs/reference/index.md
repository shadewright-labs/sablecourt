# Reference

Facts to look up, kept apart from the guides. Each page is generated from the code or checked against it by tests that `go test ./...` runs, so a page that falls behind the code fails CI. Each page says which of its parts the tests cover; the rest is written by hand. `make docs` regenerates the generated parts and `make docs-check` runs the checks.

Status: built.

- [Manifests](manifests.md): the six manifest kinds and every field with its type, whether the loader requires it, and what it means. Tested against the manifest types and the loader.
- [HTTP API](http-api.md): every route that `sablecourtd` registers, with its method, grouped by the credential that reaches it. Tested against the registered routes.
- [CLI](cli.md): every visible `sablectl` command and flag and every `sablecourtd` flag, generated from the command trees, and the environment of `sablecourt-agent`.
- [Configuration](configuration.md): the state directory, secret names, environment variables and files the daemon writes.
- [Limits](limits.md): limits, ceilings and timeouts that refuse, cut or time out a request, a task or a stored value, each with the constant that sets it. Tested against those constants.
- [Metrics](metrics.md): the Prometheus metrics at `GET /metrics`. Names and types tested against the handler.

## Status values in the tables

| Value | Meaning |
| --- | --- |
| built | Works on `main` and has tests. |
| disabled | The route or command exists and refuses every call. |
| parked | The code exists and has tests, but [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) parks feature work on it. It is not part of the supported local setup. |

Decided but unbuilt material appears under "Not built yet" on the page it affects, with the ADR link. It is never mixed into a table of working behavior.

## Related pages

- [Concepts](../concepts.md) and the [glossary](../glossary.md) explain the terms the tables use.
- [How it works](../how-it-works/index.md) explains the behavior behind each route and limit.
