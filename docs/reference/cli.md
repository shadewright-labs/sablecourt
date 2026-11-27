# CLI reference

The command-line programs: `sablectl` is the client: most commands call a running daemon over its HTTP API (or its Unix socket with `--socket`), and a few, such as `init` and `validate`, run locally; `sablecourtd` is the daemon, and `sablecourt-agent` is the agent loop that runs inside a Workload.

Status: built.

The `sablectl` and `sablecourtd` sections are generated from the command trees in `cmd/sablectl` and `cmd/sablecourtd` by `make docs`, and `go test ./...` fails when they are stale. They list every visible command with its short help and the flags it defines, with type and default. A flag is listed once, on the command that defines it; persistent flags of `sablectl` apply to every command below it. Hidden and deprecated commands (cobra's `__complete` and `__completeNoDesc` among them), hidden and deprecated flags and the `--help` flag that every command has are left out. `sablectl <command> --help` prints cobra's own help for the command.

## Operator token

There is no token flag, because a token on the command line shows in the process table. `sablectl` takes the operator token from the `SABLECOURT_OPERATOR_TOKEN` environment variable. With it unset, a client talking to a local daemon (a loopback host or `--socket`) reads `operator.token` from the [state directory](configuration.md#state-directory).

## Wait animation

While `sablectl` waits on a long event, it draws a short animated scene (7 lines by 64 columns) with a status line on standard error, so the wait shows what it waits for. The scene is cleared before each line of output and drawn again below it, and it is removed when the wait ends or on Ctrl-C, which also shows the cursor again.

| Wait | Scene |
| --- | --- |
| `sablectl council`, while the rounds run (the status shows the round and how many seats answered) | `duel` or `blade` |
| `sablectl harness build`, and the tool install of `harness install` and `harness login` | `forge` |
| `sablectl approval approve`, while the daemon waits for the out-of-band confirmation, and `sablectl auth github`, while it waits for the browser authorization | `watch` |

`sablectl task scry` and `sablectl conclave watch` print live output and draw no animation, nor does the interactive session of `harness login`.

Nothing is drawn when standard error is not a terminal or is narrower than 64 columns, with the global `--no-art` flag, or when the environment sets `SABLECOURT_NO_ART` (any value but `0`), `NO_COLOR` (any value but empty) or `TERM=dumb`. `COLORTERM=truecolor` or `24bit` selects 24-bit colour; otherwise the 256-colour palette is used.

## sablectl

<!-- BEGIN GENERATED sablectl: do not edit, run `make docs` -->

### `sablectl`

CLI client for the Sablecourt control plane

Groups the commands below; it does nothing on its own.

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--api` | string | `http://127.0.0.1:7432` | sablecourtd HTTP API base URL |
| `--json` | bool | `false` | Output raw JSON instead of styled tables |
| `--no-art` | bool | `false` | Draw no wait animation on stderr (also off with SABLECOURT_NO_ART=1, NO_COLOR, TERM=dumb or when stderr is not a terminal) |
| `--secret-file` | string | empty | Optional 0600 JSON secret file instead of OS Keychain |
| `--socket` | string | empty | Optional sablecourtd Unix domain socket path |

### `sablectl apply`

Reload manifests in a running sablecourtd instance

No flags of its own.

### `sablectl approval`

Inspect and decide runtime approval requests

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl approval approve <approval-id>`

Approve a pending request (triggers out-of-band confirmation on daemon)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--digest` | string | empty | Expected SHA-256 digest of the canonical payload (required) |

### `sablectl approval deny <approval-id>`

Deny a pending approval request

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--reason` | string | empty | Reason for denial |

### `sablectl approval list`

List pending approvals from sablecourtd

No flags of its own.

### `sablectl audit`

Inspect audit log

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl audit export`

Export audit records as JSONL to stdout

No flags of its own.

### `sablectl audit tail`

Show recent audit events from sablecourtd

No flags of its own.

### `sablectl audit verify`

Verify the tamper-evident SHA-256 hash chain across all audit records

No flags of its own.

### `sablectl auth`

Authenticate external providers via OAuth flows

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl auth github`

Authenticate GitHub via OAuth Device Flow and store github_token in SecretStore

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--client-id` | string | empty | GitHub OAuth App client_id (or set SABLECOURT_GITHUB_CLIENT_ID) |

### `sablectl budget`

Inspect observed model budget usage

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl budget status`

Show aggregate token and micro-USD usage

No flags of its own.

### `sablectl completion`

Generate the autocompletion script for the specified shell

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl completion bash`

Generate the autocompletion script for bash

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablectl completion fish`

Generate the autocompletion script for fish

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablectl completion powershell`

Generate the autocompletion script for powershell

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablectl completion zsh`

Generate the autocompletion script for zsh

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablectl conclave`

Convene and direct multi-agent Conclaves (Vassals, Wards, Decrees, Relics)

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl conclave close <conclave-id>`

Close a Conclave session

No flags of its own.

### `sablectl conclave decree <conclave-id> <message>`

Issue an operator Decree to direct Vassals or resume a Ward-paused Conclave

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--to` | string | `all` | Target Vassal role or all |

### `sablectl conclave inspect <conclave-id>`

Inspect a Conclave session, message log, and Relics

No flags of its own.

### `sablectl conclave list`

List Conclave sessions

No flags of its own.

### `sablectl conclave open <template>`

Convene a new Conclave from a manifest template

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--decree` | string | empty | Optional opening operator Decree |
| `--title` | string | empty | Conclave session title |

### `sablectl conclave relic`

Store and fetch shared Relics inside a Conclave

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl conclave relic get <conclave-id> <name>`

Fetch a Relic from a Conclave

No flags of its own.

### `sablectl conclave relic put <conclave-id> <name>`

Store a Relic (reads stdin if --body is omitted)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--body` | string | empty | Relic text body (reads stdin when omitted) |
| `--type` | string | `text/plain` | Relic MIME content type |

### `sablectl conclave watch <conclave-id>`

Watch live Conclave messages in real time

No flags of its own.

### `sablectl council [question]`

Convene a bounded multi-agent council, stream round answers, and print the digest

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--live` | bool | `false` | Use a read-only view of the live working tree (mutually exclusive with --ref) |
| `--ref` | string | empty | Pin a read-only checkout to a Git ref (mutually exclusive with --live) |
| `--repo` | string | `.` | Repository directory path |
| `--rounds` | int | `2` | Maximum council rounds |
| `--sequential` | bool | `false` | Run participants sequentially within each round |
| `--with` | string | `claude,codex,agy` | Comma-separated participant harnesses (claude,codex,agy) |
| `--writer` | seat | empty | Give one panel seat (claude, codex or agy) a writable worktree; without --ref it pins HEAD (excludes --live) |

### `sablectl device`

List and revoke paired ECDSA P-256 signing devices

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl device list`

List paired signing devices

No flags of its own.

### `sablectl device revoke <device-id>`

Revoke a paired device

No flags of its own.

### `sablectl doctor`

Check daemon health, audit chain, runtimes, coding harnesses, providers, and voice backends

No flags of its own.

### `sablectl harness`

Build local council harness images and log in to them

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl harness build <role>`

Build a pinned harness image and its proxy

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--source` | string | `.` | Sablecourt source checkout containing the council proxy |

### `sablectl harness install <role>`

Install the pinned claude or agy tool into its harness tool volume

No flags of its own.

### `sablectl harness login <role>`

Log in to a harness inside its container and keep the login in its volume

No flags of its own.

### `sablectl help [command]`

Help about any command

No flags of its own.

### `sablectl init`

Generate a starter sablecourt.yaml manifest tailored to the current repository

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--force` | bool | `false` | Overwrite existing manifest file |
| `--out` | string | `sablecourt.yaml` | Output manifest file path |

### `sablectl mcp-server`

Run Sablecourt as a stdio MCP server for Claude Code, Codex, or IDE agents

No flags of its own.

### `sablectl memory`

Manage persistent Court Memory shared across tasks and Conclaves

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl memory delete <namespace> <key>`

Delete a Court Memory entry

No flags of its own.

### `sablectl memory list`

List or search Court Memory entries

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--namespace` | string | empty | Optional memory namespace filter |
| `--q` | string | empty | Optional substring search |

### `sablectl memory set <namespace> <key> <value>`

Store a redacted Court Memory entry

No flags of its own.

### `sablectl pair`

Generate a single-use pairing code and URI for an iOS or CLI signing device

No flags of its own.

### `sablectl promote <workload>`

Create a GitOps promotion branch to graduate a sandbox workload to runtime

No flags of its own.

### `sablectl provider`

Inspect connected LLM providers and sync available models

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl provider list`

List provider connection status and cached models

No flags of its own.

### `sablectl provider sync <provider-id>`

Query upstream /models endpoint and refresh cached model list

No flags of its own.

### `sablectl secret`

Manage upstream gate credentials in OS Keychain or 0600 file

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl secret check <name>`

Check whether a secret exists without printing its value

No flags of its own.

### `sablectl secret delete <name>`

Delete a stored secret

No flags of its own.

### `sablectl secret set <name>`

Store a secret (reads from stdin if --value is omitted)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--value` | string | empty | Secret value (prefer stdin pipe over shell args) |

### `sablectl service`

Install and manage sablecourtd as a background service (macOS launchd or Ubuntu systemd)

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl service install`

Install sablecourtd as a user background service on macOS (launchd) or Linux/Ubuntu (systemd)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--db` | string | empty | SQLite database path |
| `--listen` | string | `127.0.0.1:7432` | Daemon listen address |
| `--manifests` | string | empty | Manifests directory or file |

### `sablectl tailscale`

Expose local sablecourtd privately to your Tailscale tailnet via tailscale serve

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl tailscale expose`

Expose local sablecourtd over HTTPS inside your Tailscale tailnet (no public port)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--port` | int | `7432` | Local sablecourtd port |

### `sablectl tailscale status`

Show current Tailscale serve status

No flags of its own.

### `sablectl task`

Submit and inspect tasks

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl task cancel <task-id>`

Cancel an active task and revoke its gate leases

No flags of its own.

### `sablectl task diff <task-id>`

Print the colorized git-diff artifact recorded for a coding task

No flags of its own.

### `sablectl task inspect <task-id>`

Inspect a task, its state transitions, and budget usage

No flags of its own.

### `sablectl task list`

List tasks from sablecourtd

No flags of its own.

### `sablectl task resume <task-id>`

Restore checkpoint and resume a suspended task

No flags of its own.

### `sablectl task retry <task-id>`

Re-submit an interrupted or failed task (use --handoff to attach failure-handoff.md)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--handoff` | bool | `false` | Attach standardized failure-handoff.md from artifacts and events |

### `sablectl task scry <task-id>`

Stream live redacted output from a running task (read-only)

No flags of its own.

### `sablectl task submit <workload>`

Submit a new task to sablecourtd

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--idempotency-key` | string | empty | Optional idempotency key |
| `--input` | string | `{}` | JSON task input or plain prompt text |
| `--model` | string | empty | Optional model ID |
| `--provider` | string | empty | Optional LLM provider (openai, anthropic, gemini, local) |

### `sablectl task suspend <task-id>`

Checkpoint and suspend a working task

No flags of its own.

### `sablectl task takeover <task-id>`

Attach to a running task with exclusive stdin control (Ctrl-C to detach)

No flags of its own.

### `sablectl validate <path>`

Validate Sablecourt YAML manifests

No flags of its own.

### `sablectl version`

Print sablectl version

No flags of its own.

### `sablectl voice`

Local-first Speech-to-Text and Text-to-Speech commands

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl voice speak <text>`

Synthesize and play speech audio via local TTS (Kokoro/Piper or macOS say)

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--out` | string | empty | Output audio file path (e.g. reply.wav) |
| `--voice` | string | empty | Optional TTS voice name (e.g. Zosia, Samantha, af_heart) |

### `sablectl voice status`

Show configured local STT and TTS backends

No flags of its own.

### `sablectl workload`

Inspect registered workloads

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablectl workload list`

List registered workloads from sablecourtd

No flags of its own.

<!-- END GENERATED sablectl -->

## sablecourtd

<!-- BEGIN GENERATED sablecourtd: do not edit, run `make docs` -->

### `sablecourtd`

Sablecourt local-first and enterprise Kubernetes control plane daemon

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--allow-host` | stringSlice | none | Additional allowed HTTP host or host:port (repeatable, bare hosts use the listen port) |
| `--coding-branch-retention` | duration | `0s` | Age before merged coding branches can be pruned (0 disables pruning) |
| `--db` | string | `sablecourt.db` | SQLite database path |
| `--gate-host` | string | empty | Optional host:port injected into containers for gates (defaults to --listen) |
| `--incident-conclave-template` | string | empty | Default Conclave template for new Shadewright incidents |
| `--k8s-namespace` | string | `default` | Kubernetes namespace when --runtime=k8s |
| `--k8s-runtime-class` | string | empty | Optional Kubernetes RuntimeClassName (e.g. gvisor, kata) |
| `--kubeconfig` | string | empty | Optional kubeconfig path when --runtime=k8s |
| `--listen` | string | `127.0.0.1:7432` | HTTP listen address |
| `--manifests` | string | empty | Path to a YAML manifest file or a directory of YAML manifests |
| `--manifests-git-branch` | string | `main` | Branch for --manifests-git-repo |
| `--manifests-git-interval` | duration | `1m0s` | Poll interval for --manifests-git-repo |
| `--manifests-git-repo` | string | empty | Optional Git repository URL to sync manifests from automatically |
| `--max-concurrent` | int | `4` | Maximum number of concurrently running tasks (0 = unlimited) |
| `--mode` | string | `sanctum` | Operating mode: sanctum (local macOS/Ubuntu) or citadel (Kubernetes) |
| `--model-gateway-url` | string | empty | Optional upstream AI gateway URL (e.g. agentgateway or Agent Router) |
| `--postgres-dsn` | string | empty | Optional Postgres DSN (or set SABLECOURT_POSTGRES_DSN) instead of SQLite |
| `--repo-root` | string | `.` | Git repository root for GitOps typed actions |
| `--runtime` | string | `docker` | Container runtime backend: docker or k8s |
| `--secret-file` | string | empty | Optional 0600 JSON secret file fallback path |
| `--socket` | string | empty | Optional Unix domain socket path for local CLI access (0600) |

### `sablecourtd completion`

Generate the autocompletion script for the specified shell

Groups the commands below; it does nothing on its own.

No flags of its own.

### `sablecourtd completion bash`

Generate the autocompletion script for bash

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablecourtd completion fish`

Generate the autocompletion script for fish

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablecourtd completion powershell`

Generate the autocompletion script for powershell

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

### `sablecourtd completion zsh`

Generate the autocompletion script for zsh

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-descriptions` | bool | `false` | disable completion descriptions |

<!-- END GENERATED sablecourtd -->

## sablecourt-agent

Source: `cmd/sablecourt-agent/main.go`. The program that a Workload image can run as its agent loop; the starter manifest from `sablectl init` names an image `sablecourt-agent:0.1.0` for it, which you build locally with `make agent-image` (`docker build --target agent -t sablecourt-agent:0.1.0 .`). You do not run it by hand. It has no flags and reads its environment: `SABLECOURT_TASK_TOKEN` and `SABLECOURT_MODEL_GATE_URL` are required (it exits 1 without them), and `SABLECOURT_TASK_ID`, `SABLECOURT_WORKLOAD`, `SABLECOURT_DATA_GATE_URL` and `SABLECOURT_INPUT_JSON` are optional. The task input may carry `provider` (default `anthropic`), `model` and `prompt`.

## Not built yet

Decided in [ADR-0014](../adr/0014-identity-and-authentication.md), which is Proposed: `sablectl session revoke` and `sablectl auth recover`. Neither command exists on `main`.

## In depth

- The [how-to guides](../how-to/index.md), starting with [install and start](../how-to/install-and-start.md), for the commands in context.
- [HTTP API](http-api.md) for the routes behind each command, [configuration](configuration.md) for the environment, and [troubleshooting](../troubleshooting.md).
