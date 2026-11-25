# Reference

Facts to look up, kept apart from the guides. Every table names its source file, and each page was written from the code on `main`.

Status: built.

- [Manifests](manifests.md): the six manifest kinds, every field, its default and its validation errors.
- [HTTP API](http-api.md): every route that `sablecourtd` registers, grouped by the credential that reaches it.
- [CLI](cli.md): every `sablectl` command and flag, every `sablecourtd` flag, and the environment of `sablecourt-agent`.
- [Configuration](configuration.md): the state directory, secret names, environment variables and files the daemon writes.
- [Limits](limits.md): the limits, ceilings and timeouts that refuse, cut or time out a request, a task or a stored value, with the default and the source file.
- [Metrics](metrics.md): the Prometheus metrics at `GET /metrics`.

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
