# Building blocks

This page names each part of Sablecourt once: what it is, what it is for and one example. Each section links to the page that holds the exact behaviour. Read it before the other how-it-works pages, or keep it open next to them.

Status: partly built. See "Not built yet".

## The configuration this page describes

Everything below describes one setup: a Mac with a Docker-compatible engine running, the manifests that `sablectl init` writes, and the daemon started plainly (`sablecourtd --manifests <file>`) or by `sablectl service install`, which adds only `--mode=sanctum` and file locations. That leaves the mode at `sanctum`, the runtime at `docker`, the store in a SQLite file, the listener on `127.0.0.1:7432`, no external model gateway, no secret file and no `SABLECOURT_` environment variable set.

The starter manifests declare a `coder` workload that runs a harness in a harness container, a `reviewer` workload that runs the built-in agent in a container, and a Conclave in which the two cooperate.

Several switches change which part does what. They are listed in "Switches that change this picture" at the end; read that table before applying a sentence from this page to another setup.

[![In the default configuration, manifests declare a workload; callers submit a task for it; a council attempt maps to one task; the runtime adapter chosen by the workload class starts a harness container for a coding task or council turn, or a Docker container; host mode is off unless forced.](../diagrams/building-blocks-nesting.svg)](../diagrams/building-blocks-nesting.svg "Open full size")

## Daemon

`sablecourtd` is the one long-running process. It reads the manifests, keeps the store, decides which task runs next, starts workloads and watches them, and serves the API, the web UI and the gates. The gates are part of this process, not separate services.

Example: `sablectl service install` registers the daemon as a user service, so it starts at login. See [reference](../reference.md).

## CLI

`sablectl` is the command line for people. Most of its commands are requests to the daemon's API and run nothing themselves; a few act on the local machine, such as `init`, which writes starter manifests, and `harness build` and `harness login`, which prepare the council harnesses.

Example: `sablectl task submit coder --input "fix the failing test"` asks the daemon to run the `coder` workload once. See [reference](../reference.md).

## Manifests

A manifest is a YAML file that declares what the daemon may run and under which rules. Its kinds are workloads, policies, budgets, workflows, schedules and Conclave templates. The daemon reads manifests and never writes them; everything that happens at run time goes to the store.

Example: the starter `Policy` denies everything by default and allows `coder` the memory and Conclave tools and `reviewer` the Conclave tools. See [reference](../reference.md).

## Workload

A workload is a declaration of something the daemon may launch: what kind of program it is (its class), how far it is trusted (its zone) and what it may use (its grants). A workload runs nothing by itself. The three classes are a coding agent (a harness), a long-lived agent (a container program such as the built-in agent) and a deterministic controller.

Example: the starter `coder` workload names a harness and the repository it works in. See [workloads and runtimes](workloads-and-runtimes.md).

## Task

A task is one run of one workload, with its input, its state and its results. People submit tasks through `sablectl` or the web UI; workflow steps, schedules and Conclaves submit them too, as do the webhook and A2A routes. A running task can start a child task when policy allows it, and retrying a task creates a new task.

Example: the `sablectl task submit` call above creates one task, which the daemon launches with its own lease. See [tasks](tasks.md).

## Attempt

Outside councils, "attempt" is not an object of its own: a retry is a new task, and the original task keeps its state. In a council, an attempt is one launch of one member's turn in one round, and each attempt is its own task.

Example: `codex`'s turn in round 2 fails; its retry is attempt 2 of that turn and a new task. See [council](council.md).

## Runtime adapter

A runtime adapter is the part of the daemon that turns a task into something running, watches it and stops it. The workload's class picks the adapter. In the default configuration a harness workload's task and a council turn both run in a harness container built by the same function, and a long-lived agent or a deterministic controller runs in a Docker container.

Example: the `reviewer` task starts a container from the `sablecourt-agent` image. See [workloads and runtimes](workloads-and-runtimes.md).

## Harness

A harness is a vendor's coding CLI that Sablecourt launches as a black box: `claude`, `codex` or `agy`. It runs its own agent loop, edits files and calls its vendor's model with its own login. Sablecourt chooses where it runs, which checkout it sees, what prompt it gets and which lease it holds, and it does not see the model calls the harness makes.

Example: in a council asked about a pinned commit, `claude` runs in its own container against a read-only checkout of that commit. See [harness containers](harness-containers.md).

## Built-in agent

The built-in agent is Sablecourt's own small agent program, shipped as a container image. It holds no provider key: every model call it makes goes through the Model Gate, which counts the call against the workload's budget. Between model calls it can use the tools and typed actions its workload is granted.

Example: the starter `reviewer` workload runs the built-in agent to review diffs in the Conclave. See [model gate](model-gate.md).

## Deterministic controller

A deterministic controller is a container workload for a program that needs no model, such as a check that runs after a change. Sablecourt gives it no access to the Model Gate. It does not look inside the image, so that refusal bounds what the controller can get from Sablecourt, not what the program does. It can still use tools within its grants.

Example: the `health-controller` step of the `issue-to-pr` workflow in `examples/local-demo.yaml` runs an Alpine image with no Model Gate grant. See [workloads and runtimes](workloads-and-runtimes.md).

## Gates

A gate is a checkpoint in the daemon that a workload's requests pass through. Each gate checks the task's lease, so a workload acts only as the task it belongs to. The Model Gate handles inference and adds the provider key, so the workload never holds it. The MCP Gate handles tool calls and typed actions and asks the policy engine, which can send an action to a person for approval. A harness container reaches the outside only through its council proxy, a small container that tunnels to that harness's provider and forwards calls to the gates.

Example: the built-in agent sends a chat request with its lease to the Model Gate, which checks the budget, adds the key and forwards it to the provider. See [how it works](index.md).

## Lease

A lease is the short-lived credential a task holds, bound to that task and its workload. The gates accept it in place of any real secret, and the store keeps only its hash. It is revoked when the task ends, is cancelled or is suspended; resuming a suspended task mints a new one.

Example: a council turn receives its lease when the container starts and presents it on every gate call. See [tasks](tasks.md).

## Vassal, Conclave and council

A Conclave is a room in which several workloads cooperate, opened from a Conclave manifest, with limits on messages and runs (its Wards) and a budget. A Vassal is a seat in a room: a role bound to a workload, and each time a Vassal is activated the daemon submits a task for it. A council is a room the daemon opens for `sablectl council`, without a manifest: its Vassals are the three harnesses, and it runs in bounded rounds.

Example: the starter Conclave seats `coder` and `reviewer`; `sablectl council "is this migration safe?"` opens a council of `claude`, `codex` and `agy`. See [Conclaves](conclaves.md).

## Store and audit

The store holds runtime state: tasks and their events, leases as hashes, budget reservations and spend, approvals, rooms, memory and the audit log. The audit log is a hash chain: each row carries the hash of the one before it, so a changed row breaks verification.

Example: `sablectl audit verify` checks the chain. See [ADR-0009](../adr/0009-sanctum-citadel-audit-chain-and-crypto-approvals.md).

## Integrations

Integrations are the routes through which the sibling products talk to Sablecourt. Shadewright sends incidents and verifications, which the daemon records in a room. Nightloom only reads: it gets a view of running tasks.

Example: an incident from Shadewright opens a room from a Conclave template, or joins the room already open for that incident, and appears there as a message from the Shadewright integration. See [integrations](integrations.md).

## How a harness run differs from a model call

[![In the default configuration there are two paths to a model: a harness in a council container reaches its provider through the council proxy with its own login, while the built-in agent reaches the provider through the Model Gate, which holds the key and writes the budget ledger and audit.](../diagrams/inference-paths.svg)](../diagrams/inference-paths.svg "Open full size")

The comparison is for the default configuration: a coding task or council turn in a harness container, and the starter `reviewer` workload calling a cloud provider through the Model Gate.

| Step | Harness with its own login | Built-in agent through the Model Gate |
| --- | --- | --- |
| Who holds the provider credential | The harness, in a login volume made inside its container. The daemon does not read it. | The daemon, in its secret store. The agent holds only the lease. |
| What the workload is given | A prompt, a checkout and the lease. No Model Gate address. | The Model Gate address, the lease and its input. |
| Path to the provider | Through the council proxy, which tunnels to that harness's provider hosts; TLS runs end to end between the harness and the provider. | To the Model Gate in the daemon, which forwards to the provider's HTTPS endpoint with the key. |
| What the daemon sees | The gate calls the harness makes, the turn's exit code, output and artifacts. Not the prompts, replies or token counts. | Every request and reply, including the model name and token usage the reply reports. |
| Budget | No Model Gate budget. The turn is bounded by the council's rounds and deadline and the container's resource limits. | The workload's budget: the gate reserves an estimate before forwarding and settles it with reported usage. |
| Audit | No record of provider traffic. | The gate records model calls in the audit log; the [model gate](model-gate.md) page lists which outcomes leave no row. |

[ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) accepts this as a narrow exception to invariant 2: only the three named harnesses may use their own provider login, and that inference runs outside Model Gate accounting.

## Switches that change this picture

Each row is a setting that changes which part holds a credential, which adapter launches a workload, what the adapter creates, which path inference takes or what is recorded.

| Switch | Default | What changes |
| --- | --- | --- |
| `--mode`, or `SABLECOURT_MODE` | `sanctum` | `citadel` (or `enterprise`) turns runtime `docker` into `k8s`, moves the listener from `127.0.0.1:7432` to `0.0.0.0:7432`, registers no host coding adapter, no council containers and no council panel, and drops the macOS approval dialog. The environment variable is read when the flag is `sanctum`, `local` or empty, so it overrides the `--mode=sanctum` that `sablectl service install` writes. |
| `--runtime` | `docker` | `k8s` gives the long-lived agent and deterministic controller classes to the Kubernetes adapter, which creates a Job; its Deployment path needs a `runtime.type` that the manifest loader refuses today. If the Kubernetes setup fails, those two classes have no adapter; under `docker`, a Docker client that cannot be configured stops the daemon. Harness workloads are not affected. |
| `--k8s-namespace`, `--k8s-runtime-class`, `--kubeconfig` | `default`, none, none | Only under `k8s`: where the Jobs go, and a RuntimeClass (for example gVisor or Kata) for their pods. |
| A Docker engine: `DOCKER_HOST`, or a socket of Docker Desktop, OrbStack, Colima, Rancher Desktop or Podman at its usual path | found when one is running | Without one, coding tasks and councils are refused: there is no fallback to the host unless `--unsafe-host-harnesses` is set. |
| `--unsafe-host-harnesses` | off | With no container runtime, coding tasks and council turns run as host processes under Seatbelt, with no proxy and no login volume. Such runs are outside invariants 2 and 3. The daemon logs a warning, and `sablectl doctor` shows the mode. With a container runtime present, containers are still used and host launches are refused, though the daemon still probes Seatbelt, logs the startup warning, and `sablectl doctor` notes the unused opt-in. Standalone coding workloads take the host path only when `spec.coding.sandbox` is `seatbelt`. |
| Host platform | macOS | Host mode turns on only when the Seatbelt probe passes. On Linux, without a container runtime, coding tasks and councils are refused even with `--unsafe-host-harnesses`. |
| `--model-gateway-url`, or `SABLECOURT_MODEL_GATEWAY_URL` | none | The Model Gate sends every provider's requests to that gateway, and no provider key is required in the daemon: the gateway can hold them. A key that is in the secret store is still added. |
| Provider `local` in the task input | `anthropic` for the built-in agent | The Model Gate forwards to `http://127.0.0.1:11434` over plain HTTP and needs no key; `local_model_api_key` is added if stored. |
| `--secret-file` | none | Secrets are read from the system keyring first and then from this 0600 JSON file. |
| `--postgres-dsn`, or `SABLECOURT_POSTGRES_DSN` | SQLite at `--db` | Tasks, leases, budgets, rooms and the audit log are kept in PostgreSQL. |
| `--gate-host` | `host.docker.internal:<port>` while the daemon listens on loopback | The gate address given to container workloads. Host processes do not use it. |
| Room type: council or manifest Conclave | (per room) | In a council the daemon posts each completed harness turn's final message to the room. In a Conclave opened from a manifest it does not (#28); a Vassal's message reaches the room only when the Vassal posts it through the Conclave tools of the MCP Gate. |
| Checkout: plain coding task, or council `live`, `ref` or `writer` | plain task: a worktree of the workload's repository | A plain coding task gets a worktree and a `.sablecourt-session.json` marker file in it. A council turn writes no marker: `live` gives a read-only view of the working tree, `ref` a read-only checkout of one commit, and `writer` gives one role a writable worktree on its own branch. |

Flags not listed (`--listen`, `--socket`, `--allow-host`, `--db`, `--manifests` and the `--manifests-git-*` flags, `--max-concurrent`, `--coding-branch-retention`, `--repo-root`, `--incident-conclave-template`) and the variables `SABLECOURT_STATE_DIR`, `SABLECOURT_PUBLIC_URL` and `SABLECOURT_NOTIFY_WEBHOOK_URL` change addresses, locations, limits or notifications, not which part does what.

## Not built yet

- Posting a harness Vassal's final message to a room opened from a manifest Conclave (#28); councils already do.
- Checking subscription windows before a council round (#36).
- Coding tasks on a machine without a container runtime. The 2026-11-27 amendment of [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md#amendment-2026-11-27-container-only-harnesses-for-010) drops host mode as a supported path for 0.1.0; forced host mode gives a harness an empty scratch home directory and no login (#17). Separate harness state directories for host mode are dropped by the same amendment.
- Checkpointed suspend and resume (#14).
- An audit chain that detects deletion of its newest rows (#10).
- A status and learnings the Shadewright side can read (#53, #54).

## In depth

- [Concepts](../concepts.md), [glossary](../glossary.md)
- [Workloads and runtimes](workloads-and-runtimes.md), [harness containers](harness-containers.md), [council](council.md), [model gate](model-gate.md)
- [ADR-0002](../adr/0002-heterogeneous-mvp-workload-adapters.md), [ADR-0004](../adr/0004-in-house-capability-gates-three-doors.md), [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md), [ADR-0013](../adr/0013-bounded-council-rounds.md)
