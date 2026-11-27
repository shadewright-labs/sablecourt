# Configuration reference

Where `sablecourtd` and `sablectl` keep state, which secrets and environment variables they read, and what they pass to workloads. Command-line flags are in the [CLI reference](cli.md).

Status: built. A row marked parked belongs to feature work that [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) parks: PostgreSQL, A2A, GitHub integrations, upstream MCP, voice, device pairing and chat alerts.

## State directory

Source: `internal/operatortoken/token.go` and `cmd/sablecourtd/main.go`. The directory is `SABLECOURT_STATE_DIR` if set (it must be an absolute path), otherwise `.local/share/sablecourt` under the home directory. It is created with mode 0700.

`TestConfigurationStateDirectory` in `internal/refdocs` checks this section: it calls the code that picks and creates the directory with a temporary home directory and fails when the default path, the directory mode, or the name and mode of an entry the daemon creates differ from this page.

| Entry | Purpose |
| --- | --- |
| `operator.token` | The operator token, `op_` followed by 64 hex characters, mode 0600. The daemon creates it on first start. `sablectl` reads it when it talks to a local daemon and has no other token. |
| `operator.token.pending` | Short-lived file the daemon writes while it creates `operator.token`, mode 0600. |
| `daemon.lock` | Lock file, mode 0600. A second daemon on the same directory refuses to start. |
| `gates/` | Council gate sockets. A council launch in Unix socket mode keeps its `gate.sock` here until cleanup; a launch in host-gateway mode uses a TLS gate and keeps no socket. |

## Other files

Source: `cmd/sablecourtd/main.go`, `cmd/sablectl/ops.go`, `cmd/sablectl/dx.go`, `internal/runtime/coding/worktree.go`.

| File | Written by | Notes |
| --- | --- | --- |
| `sablecourt.db` | `sablecourtd` | SQLite database. Path is `--db`, relative to the working directory by default. Its directory must exist, the file must not be a symlink and the path must not contain `?`; the daemon creates a missing file at startup and refuses to start if the file, the secret file or the state directory lies under the worktree root (`worktrees` beside the database). `sablectl service install` sets it to `sablecourt.db` in `.local/share/sablecourt` under the home directory, which is also the default state directory (the command ignores `SABLECOURT_STATE_DIR`). |
| `worktrees/` next to the database | Coding adapter | Per task: `<task>/` (the Git worktree), `<task>.state.json`, `<task>.log`, `<task>.run/`, `<task>.exclude-config`, plus a `.councils/` directory and `.coding-scratch-*` directories. |
| `.sablecourt-session.json`, `.sablecourt-context.md` | Coding adapter | Written into the task worktree. |
| `sablecourtd.out.log`, `sablecourtd.err.log` | The launchd job (macOS only) | Standard output and error of the job, in `.local/share/sablecourt` under the home directory. The Linux systemd unit sets no log files. |
| `Library/LaunchAgents/io.shadewright.sablecourtd.plist`, `.config/systemd/user/sablecourtd.service` | `sablectl service install` | The service definition, mode 0600, under the home directory: the plist on macOS, the unit on Linux. |
| `.config/sablecourt/manifests` under the home directory | You | Last place the daemon looks for manifests. |
| `sablecourt.yaml` | `sablectl init` | Starter manifest in the working directory, mode 0600. |

## Secrets

Source: `internal/secret`. The daemon reads a secret from the OS keyring (service name `sablecourt`), then from the file given by `--secret-file`. That file's directory must exist and the file must not be a symlink; the daemon creates a missing one at startup as an empty JSON map with mode 0600. `sablectl secret set <name>` writes the keyring, or the file when `--secret-file` is given. Workloads never see these values.

| Name | Used by | Required for |
| --- | --- | --- |
| `openai_api_key` | Model Gate, provider `openai` | Calls to OpenAI, unless `--model-gateway-url` is set. |
| `anthropic_api_key` | Model Gate, provider `anthropic` | Calls to Anthropic, same exception. |
| `gemini_api_key` | Model Gate, provider `gemini` | Calls to Gemini, same exception. |
| `local_model_api_key` | Model Gate, provider `local` | Optional. The local endpoint may need no key. |
| `shadewright_bearer_token` | Integration routes | The two `/api/v1/integrations/shadewright/` routes. |
| `observer_bearer_token` | Observer routes | `GET /api/v1/observer/*` and the Scry stream for that credential. |
| `github_webhook_secret` | `POST /api/v1/hooks/github` | Parked: [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) parks GitHub integrations. |
| `a2a_bearer_token` | `POST /a2a` | Parked. |
| `github_token` | `git.open_pr` action, `sablectl auth github` | Parked. |
| `notify_webhook_url` | Chat alerts | Parked. Also read from `SABLECOURT_NOTIFY_WEBHOOK_URL`. |
| `secretName` of an upstream MCP server | Upstream MCP calls | Parked. The name comes from the manifest. |

Voice settings resolve in the order secret, environment variable, default (source: `internal/voice/voice.go`). All voice routes are parked.

| Secret | Environment variable | Default |
| --- | --- | --- |
| `voice_stt_url` | `SABLECOURT_STT_URL` | `http://127.0.0.1:8080/v1/audio/transcriptions` |
| `voice_stt_model` | `SABLECOURT_STT_MODEL` | `whisper-large-v3-turbo` |
| `voice_tts_url` | `SABLECOURT_TTS_URL` | none |
| `voice_tts_model` | `SABLECOURT_TTS_MODEL` | `kokoro` |
| `voice_tts_voice` | `SABLECOURT_TTS_VOICE` | `af_heart` when a TTS URL is set; otherwise none, so `say` on macOS uses its own voice |

## Model Gate upstreams

Source: `internal/gate/model/gate.go`. With `--model-gateway-url` set, every provider goes to that URL and needs no secret.

| Provider | Default upstream | Secret | Allowed paths below `/gate/model/<provider>` |
| --- | --- | --- | --- |
| `openai` | `https://api.openai.com` | `openai_api_key` | `/v1/chat/completions`, `/v1/responses` |
| `anthropic` | `https://api.anthropic.com` | `anthropic_api_key` | `/v1/messages` (header `anthropic-version: 2023-06-01`) |
| `gemini` | `https://generativelanguage.googleapis.com` | `gemini_api_key` | `/v1beta/models/<model>:generateContent`, `:streamGenerateContent` |
| `local` | `http://127.0.0.1:11434` | `local_model_api_key` (optional) | `/v1/chat/completions`, `/v1/responses` |

## Environment variables

Source: `internal/operatortoken/token.go`, `cmd/sablecourtd/main.go`, `cmd/sablectl/main.go`, `internal/notify/notify.go`, `internal/runtime/docker/docker.go`, `internal/harnessimages`, `internal/runtime/coding`, `internal/waitart`.

The tables in this section and the Environment variable column of the voice table are checked by `TestConfigurationEnvironmentVariables` in `internal/refdocs`: it finds every `os.Getenv`, `os.LookupEnv` and `os.Environ` call in `cmd/sablecourtd`, `cmd/sablectl` and `internal`, and fails when a name the code reads is missing here or a name here is not read. The "Read by" and "Passed by" columns are not checked. No flag takes its default from the environment through a binding library; each fallback is one of these calls.

| Variable | Read by | Meaning |
| --- | --- | --- |
| `SABLECOURT_STATE_DIR` | both | State directory, absolute path. |
| `SABLECOURT_MODE` | `sablecourtd` | Mode, used only when `--mode` is empty, `sanctum` or `local`. |
| `SABLECOURT_MODEL_GATEWAY_URL` | `sablecourtd` | Same as `--model-gateway-url`. |
| `SABLECOURT_POSTGRES_DSN` | `sablecourtd` | Same as `--postgres-dsn`. [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md) parks PostgreSQL. |
| `SABLECOURT_OPERATOR_TOKEN` | `sablectl` | The operator token. Without it, `sablectl` reads `operator.token` from the state directory when the daemon is local. Sent only over HTTPS or to a local daemon. There is no matching flag. |
| `SABLECOURT_GITHUB_CLIENT_ID` | `sablectl` | Same as `auth github --client-id` (parked). |
| `SABLECOURT_NO_ART` | `sablectl` | Any value but empty or `0` turns off the [wait animation](cli.md#wait-animation), as `--no-art` does. |
| `NO_COLOR` | `sablectl` | Any value but empty turns off the wait animation. |
| `TERM` | `sablectl` | `dumb` turns off the wait animation. |
| `COLORTERM` | `sablectl` | `truecolor` or `24bit` draws the wait animation in 24-bit colour; any other value uses the 256-colour palette. |
| `SABLECOURT_PUBLIC_URL` | `sablecourtd` | Base URL placed in chat alerts (parked). |
| `DOCKER_HOST` | `sablecourtd`, `sablectl harness login`, `sablectl harness install`, `sablectl harness build` | Docker endpoint, as for the Docker client. It must be a local `unix://` socket or Windows named pipe (`npipe://`). On a remote engine (`tcp://`, `ssh://` or any other host), `sablecourtd` with the Docker backend, `sablectl harness login` and `sablectl harness install` refuse to start. The check reads only the host string, so a local socket that forwards to a remote engine is not detected. `sablectl harness build` does not make this check: it passes the variable to `docker` (see below), so an image build can run on a remote engine, with no bind mount and no login volume. |
| `SABLECOURT_NOTIFY_WEBHOOK_URL` | `sablecourtd` | Chat alert webhook, used when the `notify_webhook_url` secret is not set (parked). |

Some names are read only to copy them into the environment of a program the daemon or `sablectl` starts. Writable Git commands in `internal/runtime/coding/git.go` get the whole environment without the `GIT_*` variables. The table lists only names copied one by one; a child process started without an explicit environment inherits the whole one, and that is not listed.

| Variable | Passed by | To |
| --- | --- | --- |
| `PATH` | Git commands of councils and coding tasks, `sablectl harness build` | `git`, `docker`, `go` |
| `HOME` | `sablectl harness build` | `docker`, and `go` when it builds the council proxy |
| `GOCACHE`, `TMPDIR` | `sablectl harness build` | `go` when it builds the council proxy |
| `DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`, `DOCKER_TLS_VERIFY`, `DOCKER_CERT_PATH` | `sablectl harness build` | `docker` |

`sablectl harness install` and `sablectl harness login` download the pinned `claude` or `agy` tool with Go's HTTP client, which reads `HTTPS_PROXY` and `NO_PROXY` (either case) from the environment of `sablectl`.

## Environment passed to workloads

Source: `internal/runtime/docker`, `internal/runtime/k8s`, `internal/runtime/coding`.

| Variable | Docker and Kubernetes | Host coding runtime (unsupported) | Council container |
| --- | --- | --- | --- |
| `SABLECOURT_MANAGED` | `1` | `1` | `1` |
| `SABLECOURT_TASK_ID` | task ID | task ID | task ID |
| `SABLECOURT_WORKLOAD` | workload name | workload name | workload name |
| `SABLECOURT_INPUT_JSON` | task input, when set | not passed | not passed |
| `SABLECOURT_DATA_GATE_URL` | URL of `/gate/` | same | same |
| `SABLECOURT_MODEL_GATE_URL` | URL of `/gate/model/`, when the workload holds a Model Gate grant | not passed | not passed |
| `SABLECOURT_TASK_TOKEN` | the `sc_task_` lease | same | read by the container bootstrap from standard input, then unset |
| `SABLECOURT_TASK_TOKEN_FILE` | not passed | not passed | path of a mode 0600 file in `/scratch` that holds the lease |
| `PATH`, `HOME`, `TMPDIR`, `LANG` | image defaults | fixed `PATH`, `LANG=en_US.UTF-8`, `HOME` and `TMPDIR` in the scratch directory | `PATH=/usr/local/bin:/usr/bin:/bin`, `HOME=/home/harness`, `TMPDIR=/scratch` |
| `HTTP_PROXY`, `HTTPS_PROXY` (both cases), `NO_PROXY` | not set | not set | the council gate; `NO_PROXY` names the gate host |

## Harness images

Source: `internal/harnessimages`, `cmd/sablectl/harness.go`. `sablectl harness build <role>` builds one image per council member for `linux/arm64`. Each container runs as user 10001:10001 with its state in `/home/harness`. You log each harness in once inside its container, and the login is kept in a Docker volume.

The `claude` and `agy` images carry no vendor tool; `sablectl harness install <role>` installs the pinned tool into the tool volume `sablecourt-harness-<role>-tool`, and `sablectl harness login <role>` does so when the login volume does not exist yet or the tool is missing, of another version or does not match its digest ([harness images](../how-to/harness-images.md)).

| Role | Pinned version | Login command | Login volume |
| --- | --- | --- | --- |
| `claude` | 2.1.332 | `claude auth login` | `sablecourt-harness-claude` |
| `codex` | 0.172.0 | `codex login --device-auth` | `sablecourt-harness-codex` |
| `agy` | 1.5.17 | `agy` | `sablecourt-harness-agy` |

Image tags are `sablecourt-harness-<role>:<version>-<rev>` and `sablecourt-harness-proxy:<rev>` for the proxy image.

## In depth

- [Install and start](../how-to/install-and-start.md), [log in harnesses](../how-to/log-in-harnesses.md) and [harness containers](../how-it-works/harness-containers.md).
- [Identity and access](../how-it-works/identity-and-access.md) for the tokens, and [limits](limits.md) for numeric settings.
- [Harness notes](../harnesses.md) for the original spike.
