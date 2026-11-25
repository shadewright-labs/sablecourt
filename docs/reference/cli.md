# CLI reference

The command-line tools: `sablectl` talks to a running daemon over its HTTP API, and `sablecourtd` is the daemon. Run `sablectl <command> --help` for the generated help of any command.

Status: built.

## sablectl global flags

Source: `cmd/sablectl/main.go`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--api` | `http://127.0.0.1:7432` | Base URL of the daemon. |
| `--json` | off | Print raw JSON where a command supports it. |
| `--token` | empty | Operator token. Falls back to the `SABLECOURT_OPERATOR_TOKEN` environment variable. With neither, a client talking to a local daemon reads `operator.token` from the [state directory](configuration.md#state-directory). A token is only sent over HTTPS or to a local daemon. |
| `--socket` | empty | Path of the daemon's Unix socket. Replaces `--api` and the token. |
| `--secret-file` | empty | Optional JSON secret file (mode 0600) used by the `secret` commands instead of the OS keyring. |

## sablectl commands

Source: `cmd/sablectl/*.go`. The route column names the API route that the command calls.

### Tasks and workloads

| Command | Flags and arguments | Route | Status |
| --- | --- | --- | --- |
| `task submit <workload>` | `--input` (default `{}`), `--idempotency-key`, `--provider`, `--model` | `POST /api/v1/tasks` | built |
| `task list` | none | `GET /api/v1/tasks` | built |
| `task inspect <id>` | none | `GET /api/v1/tasks/{id}` | built |
| `task cancel <id>` | none | `POST /api/v1/tasks/{id}/cancel` | built |
| `task suspend <id>` | none | `POST /api/v1/tasks/{id}/suspend` | built |
| `task resume <id>` | none | `POST /api/v1/tasks/{id}/resume` | built |
| `task retry <id>` | `--handoff` | `POST /api/v1/tasks/{id}/retry` | built |
| `task scry <id>` | none | `GET /api/v1/tasks/{id}/scry` | built |
| `task takeover <id>` | none | `POST /api/v1/tasks/{id}/takeover` and `/stdin` | parked |
| `task diff <id>` | none | `GET /api/v1/tasks/{id}`, then prints the `git-diff` artifact of a coding task | built |
| `workload list` | none | `GET /api/v1/workloads` | built |
| `promote <workload>` | none | `POST /api/v1/promote/{name}` | built |
| `budget status` | none | `GET /api/v1/budget` | built |

### Approvals, audit and devices

| Command | Flags and arguments | Route | Status |
| --- | --- | --- | --- |
| `approval list` | none (pending only) | `GET /api/v1/approvals?status=pending` | built |
| `approval approve <id>` | `--digest` (the expected payload SHA-256) | `POST /api/v1/approvals/{id}/approve` | built |
| `approval deny <id>` | `--reason` | `POST /api/v1/approvals/{id}/deny` | built |
| `audit tail` | none | `GET /api/v1/audit` | built |
| `audit verify` | none | `GET /api/v1/audit/verify` | built |
| `audit export` | none | `GET /api/v1/audit/export` (printed as JSON lines) | built |
| `device list` | none | `GET /api/v1/devices` | built |
| `device revoke <id>` | none | `POST /api/v1/devices/{id}/revoke` | built |
| `pair` | none | `POST /api/v1/devices/pair/start` (always 503) | disabled |

`approval approve` waits for the macOS confirmation dialog, so its client timeout is 65 seconds.

### Conclaves and councils

| Command | Flags and arguments | Route | Status |
| --- | --- | --- | --- |
| `conclave open <template>` | `--title`, `--decree` | `POST /api/v1/conclaves` | built |
| `conclave list` | none | `GET /api/v1/conclaves` | built |
| `conclave inspect <id>` | none | `GET /api/v1/conclaves/{id}` | built |
| `conclave watch <id>` | none (polls every 500 ms) | `GET /api/v1/conclaves/{id}` | built |
| `conclave decree <id> <message>` | `--to` (default `all`) | `POST /api/v1/conclaves/{id}/decree` | built |
| `conclave relic put <id> <name>` | `--body` (reads standard input if empty), `--type` (default `text/plain`) | `POST /api/v1/conclaves/{id}/relics` | built |
| `conclave relic get <id> <name>` | none | `GET /api/v1/conclaves/{id}/relics/{name}` | built |
| `conclave close <id>` | none | `POST /api/v1/conclaves/{id}/close` | built |
| `council [question]` | `--repo` (default `.`), `--ref`, `--live`, `--rounds` (default 2), `--with` (default `claude,codex,agy`), `--sequential` | `POST /api/v1/councils` | built |

`council` prompts for the question when none is given, and the question is at most 30 KiB. `--ref` and `--live` exclude each other; with neither, the daemon reads the working tree live. The command sends a fresh `Idempotency-Key` of the form `ccl_req_<random>`. The API fields `writer`, `starts` and `durationSeconds` have no flag. See the [body table](http-api.md#councils-and-conclaves).

### Manifests, memory, providers and secrets

| Command | Flags and arguments | Route or effect | Status |
| --- | --- | --- | --- |
| `init` | `--out` (default `sablecourt.yaml`), `--force` | Writes a starter manifest (one Budget, one Policy, Workloads `coder` and `reviewer`, one Conclave) with mode 0600. Refuses to overwrite without `--force`. | built |
| `validate <path>` | none | Loads the manifests locally and prints counts of Workloads, Policies, Budgets and Workflows. No daemon needed. | built |
| `apply` | none | `POST /api/v1/reload` | built |
| `memory list` | `--namespace`, `--q` | `GET /api/v1/memory` | built |
| `memory set <namespace> <key> <value>` | none | `POST /api/v1/memory` | built |
| `memory delete <namespace> <key>` | none | `DELETE /api/v1/memory/{namespace}/{key}` | built |
| `provider list` | none | `GET /api/v1/providers` | built |
| `provider sync <id>` | none | `POST /api/v1/providers/{id}/sync` | built |
| `secret set <name>` | `--value` (reads standard input if empty) | Stores the secret in the OS keyring. | built |
| `secret check <name>` | none | Reports whether the secret exists, never its value. | built |
| `secret delete <name>` | none | Removes it from the keyring. | built |
| `harness build <role>` | `--source` (default `.`) | Builds the Docker image for the council harness `claude`, `codex` or `agy`. See [configuration](configuration.md#harness-images). | built |

### Setup, diagnosis and integrations

| Command | Flags and arguments | Route or effect | Status |
| --- | --- | --- | --- |
| `service install` | `--listen`, `--db`, `--manifests` | Writes a launchd job `io.shadewright.sablecourtd` on macOS or a systemd user unit `sablecourtd.service` on Linux, with `--mode=sanctum`. The database goes in the data directory as `sablecourt.db`, logs in `sablecourtd.out.log` and `sablecourtd.err.log`. | built |
| `doctor` | none | Reports daemon health, audit chain validity, connected providers, and whether `docker`, `kubectl`, `ollama`, `claude`, `codex`, `agy` and `whisper-server` are on the path. | built |
| `version` | none | Prints the version. | built |
| `completion` | shell name | Prints a shell completion script. | built |
| `mcp-server` | none | Runs a stdio MCP server for an editor or agent. Tools: `sablecourt_list_workloads`, `sablecourt_submit_task`, `sablecourt_inspect_task`, `sablecourt_open_conclave`, `sablecourt_conclave_decree`, `sablecourt_inspect_conclave`, `sablecourt_budget_status`, `sablecourt_council_convene`, `sablecourt_council_read`, `sablecourt_council_say`. Arguments are limited to 256 KiB. | built |
| `voice status` | none | `GET /api/v1/voice/status` | parked |
| `voice speak <text>` | `--out`, `--voice` | `POST /api/v1/voice/speak` | parked |
| `auth github` | `--client-id` (or `SABLECOURT_GITHUB_CLIENT_ID`) | GitHub device flow, stores `github_token`. | parked |
| `tailscale expose` | `--port` (default 7432) | Publishes the daemon through Tailscale. | parked |
| `tailscale status` | none | Shows the Tailscale state. | parked |

## sablecourtd flags

Source: `cmd/sablecourtd/main.go`.

| Flag | Default | Purpose |
| --- | --- | --- |
| `--mode` | `sanctum` | `sanctum` (alias `local`) or `citadel` (alias `enterprise`, parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md)). The `SABLECOURT_MODE` variable applies only when the flag is empty, `sanctum` or `local`. |
| `--listen` | `127.0.0.1:7432` | TCP address. Citadel changes the default to `0.0.0.0:7432`. |
| `--allow-host` | none | Extra `Host` header values to accept. Repeatable. A bare host uses the listen port. |
| `--socket` | empty | Also serve on this Unix socket, mode 0600. |
| `--gate-host` | `host.docker.internal:<port>` when the listen host is loopback | Authority that containers use to reach `/gate/`. |
| `--db` | `sablecourt.db` | SQLite file. Relative to the working directory. |
| `--postgres-dsn` | empty | PostgreSQL connection string (parked). Also `SABLECOURT_POSTGRES_DSN`. |
| `--manifests` | empty | Manifest file or directory. With none, the daemon tries `manifests`, then `examples/local-demo.yaml`, then the user config directory (`.config/sablecourt/manifests` under the home directory). |
| `--manifests-git-repo` | empty | Git URL to sync manifests from. Parked with the other Git integrations. |
| `--manifests-git-branch` | `main` | Branch to sync. |
| `--manifests-git-interval` | `60s` | Sync interval. |
| `--coding-branch-retention` | `0` | How long to keep coding task branches. `0` keeps them. |
| `--max-concurrent` | `4` | Maximum tasks launched in parallel. |
| `--model-gateway-url` | empty | Send every Model Gate provider to this URL, with no provider secret. Also `SABLECOURT_MODEL_GATEWAY_URL`. |
| `--incident-conclave-template` | empty | Default Conclave template for Shadewright incidents. |
| `--secret-file` | empty | Secrets file read after the OS keyring. |
| `--repo-root` | `.` | Git root used by `promote` and the `git.create_branch` action. |
| `--runtime` | `docker` | `docker` or `k8s`. Citadel forces `k8s` when it is `docker` or empty. |
| `--k8s-namespace` | `default` | Namespace for Kubernetes workloads (parked). |
| `--k8s-runtime-class` | empty | Runtime class for Kubernetes workloads (parked). |
| `--kubeconfig` | empty | Kubeconfig path (parked). |

`--operator-auth-required` is hidden and deprecated. It is ignored, and operator authentication is always required.

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: `sablectl session revoke` and `sablectl auth recover`. Neither command exists on `main`.

## In depth

- The [how-to guides](../how-to/index.md), starting with [install and start](../how-to/install-and-start.md), for the commands in context.
- [HTTP API](http-api.md) for the routes behind each command, [configuration](configuration.md) for the environment, and [troubleshooting](../troubleshooting.md).
