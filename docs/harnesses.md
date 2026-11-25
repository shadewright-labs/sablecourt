# Coding CLI headless invocations

Verified on macOS (`darwin`) on 2026-11-25 against the installed binaries in throwaway Git repositories under `.tmp/`. Every command below was run with stdin closed (`</dev/null`), a single-line prompt, and the cheapest available model for each CLI. No global permission-bypass flags (`--dangerously-skip-permissions`, `--dangerously-bypass-approvals-and-sandbox`) and no `agy --mode plan` were used.

## Summary table

| Field | `claude` | `codex` | `agy` |
| --- | --- | --- | --- |
| Installed version | `2.1.332 (Claude Code)` | `codex-cli 0.171.0` | `1.5.16` |
| Read-only review command | `claude --model haiku --permission-mode plan --no-session-persistence --output-format text -p "<prompt>" </dev/null` | `codex exec --sandbox read-only --ephemeral -m gpt-5.6-luna -o <out-file> "<prompt>" </dev/null` | `agy --model gemini-3.8-flash-low --output-format text --sandbox -p "<prompt>" </dev/null` under `sandbox-exec` without `file-write*` on the working directory |
| Edit command | `claude --model haiku --permission-mode acceptEdits --no-session-persistence --output-format text -p "<prompt>" </dev/null` | `codex exec --sandbox workspace-write --ephemeral -m gpt-5.6-luna -o <out-file> "<prompt>" </dev/null` | `agy --model gemini-3.8-flash-low --output-format text --sandbox -p "<prompt>" </dev/null` |
| Prompt passing and closed stdin | Positional argument with `-p` / `--print` (or stdin if omitted). Runs and exits cleanly with `</dev/null`. | Positional argument to `codex exec` (or stdin when omitted or `-`). With `</dev/null`, prints `Reading additional input from stdin...` to stderr, hits EOF, and runs; an open pipe blocks until EOF. | Flag value to `-p` / `--print`, placed after other flags (or attached as `-p="<prompt>"`). Putting `--model` after `-p` causes `-p` to consume `--model` as the prompt and exit `2`. Runs cleanly with `</dev/null` when authenticated. |
| Model selection | `--model <model>` (e.g. `haiku`, `sonnet`, `opus`, `fable`) | `-m <model>` or `--model <model>` (e.g. `gpt-5.6-luna`, `gpt-6.2-sol`) | `--model <model>` (e.g. `gemini-3.8-flash-low`, listed by `agy models`) |
| Final answer location | `stdout` (final message only with `--output-format text`; `stderr` is empty on success) | `stdout` (final message only; banner and tool logs go to `stderr`) and `-o <file>` / `--output-last-message <file>` (same message, no trailing newline) | `stdout` (final message only with `--output-format text`; `stderr` is empty on success) |
| Exit codes (success / failure) | `0` on success (including plan-mode write refusal); `1` on missing auth (`Not logged in`) | `0` on success (including read-only write refusal); `1` on missing `CODEX_HOME` or `401 Unauthorized` | `0` on success (including OS-blocked write); `1` on auth timeout (60s); `2` on flag parse error |
| Writable directories outside working directory | `$HOME/.claude/`, `$HOME/.claude.json` (plus sibling atomic temp files), `$HOME/Library/Caches/claude-cli-nodejs/`, `$TMPDIR` (and `/private/tmp` if the Bash tool runs) | `$HOME/.codex/` (or `$CODEX_HOME`, for SQLite state/logs/locks, `sessions/`, `shell_snapshots/`, `skills/.system/`, `tmp/arg0-macos/`, `.tmp/`), `$TMPDIR` (and parent of `-o <file>` if outside the repo) | `$HOME/.gemini/` (`$HOME/.gemini/antigravity-cli/` for brain, builtin, cache, conversations, logs, updater, SQLite summaries; `$HOME/.gemini/config/`), `$TMPDIR` |
| Auth environment variables | `HOME` (macOS Keychain plus `$HOME/.claude.json` for subscription login), or `ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` | `HOME` or `CODEX_HOME` (directory containing `auth.json` for subscription login), or `OPENAI_API_KEY` | `HOME` (subscription OAuth state under `$HOME/.gemini/`) |
| Works under write-confined `sandbox-exec` | Yes (Write tool created `probe.txt` and exited `0`; Bash tool hit `EPERM` on `/private/tmp` when `/private/tmp` was not writable) | No with `--sandbox workspace-write` or `--sandbox read-only` (outer `sandbox-exec` blocks inner `sandbox_apply` with `Operation not permitted`, exit `71`, so `apply_patch` and shell commands fail) | Yes (read-only profile blocked `probe.txt` with `operation not permitted` and exited `0`; edit profile created `probe.txt` and exited `0`) |

## Observed behavior notes

### `claude`

- Pointing `HOME` and `XDG_*` at an empty directory exits `1` with `Not logged in · Please run /login` after creating `$HOME/.claude.json` and `$HOME/.claude/{backups,plugins,projects,sessions}`.
- `--permission-mode plan` refuses file edits in the repository and exits `0`, but still writes a markdown plan under `$HOME/.claude/plans/` even when `--no-session-persistence` is set.
- `--permission-mode acceptEdits` creates and edits files inside the working directory headlessly and exits `0` with only the final answer on `stdout`.
- Under a write-confining `sandbox-exec` profile allowing writes only to the working directory, `$TMPDIR`, `$HOME/.claude`, `$HOME/.claude.json*`, `$HOME/Library/Caches/claude-cli-nodejs`, and `/dev/null`, `claude` edited `probe.txt` via the Write tool and exited `0`. The Bash tool failed with `EPERM` trying to create a temp directory under `/private/tmp`.

### `codex`

- Pointing `CODEX_HOME` at a nonexistent directory exits `1` immediately (`Error finding codex home`). Pointing `HOME` at an empty directory with an existing empty `.codex/` directory populates SQLite state (`state_5.sqlite`, `logs_2.sqlite`, `goals_1.sqlite`, `memories_1.sqlite`, `queue_1.sqlite`, `thread_history_1.sqlite`), locks, and system skills under `.codex/`, then exits `1` with `401 Unauthorized`.
- `codex exec --sandbox read-only --ephemeral` rejects file writes (`patch rejected: writing is blocked by read-only sandbox`) and exits `0`. `codex exec --sandbox workspace-write --ephemeral` creates `probe.txt` and exits `0`.
- Both `stdout` and `-o <file>` hold only the final message; session headers, tool execution, and hook logs go to `stderr`.
- Under an outer `sandbox-exec` profile, the main `codex` process starts and talks to the API, but its inner Seatbelt sandbox (`codex sandbox macos` / `apply_patch`) fails with `sandbox-exec: sandbox_apply: Operation not permitted` (exit `71`), so edits cannot be written while both outer `sandbox-exec` and `codex`'s internal Seatbelt sandbox are active.

### `agy`

- Pointing `HOME` and `XDG_*` at an empty directory populates `$HOME/.gemini/antigravity-cli/` and `$HOME/.gemini/config/`, prints an OAuth URL to `stderr`, waits 60 seconds even with `</dev/null`, and exits `1` (`Error: authentication timed out.`).
- In print mode (`-p`), default execution edits files in the working directory even when `--sandbox` is passed (`--sandbox` restricts terminal commands, not workspace file tools).
- Read-only enforcement without `--mode plan` works via `sandbox-exec` omitting `file-write*` on the working directory: `write_file` returns `operation not permitted`, shell execution fails (`failed to create PTY: operation not permitted`), `probe.txt` is not created, and `agy` exits `0`. Allowing `file-write*` on the working directory under the same profile creates `probe.txt` and exits `0`.

## Mismatches with `internal/runtime/coding`

Compared against `internal/runtime/coding/launch.go`, `internal/runtime/coding/sandbox.go`, and `internal/runtime/coding/coding.go`:

- mismatch: launch.go:launchInput.args does not pass `--permission-mode acceptEdits`, `--no-session-persistence`, or `--output-format text` for `claude` edit runs, the CLI needs `--permission-mode acceptEdits` to edit headlessly without prompting and `--no-session-persistence --output-format text` for clean stateless output
- mismatch: launch.go:launchInput.args does not pass `--sandbox workspace-write`, `--ephemeral`, or `-o <file>` for `codex` edit runs, the CLI needs `--sandbox workspace-write` because `codex exec` defaults to `read-only` and cannot edit files without it
- mismatch: launch.go:launchInput.args passes `--mode plan` for `agy` read-only runs, the CLI needs OS-level read-only confinement on the working directory instead of `--mode plan`
- mismatch: launch.go:launchInput.args does not pass `--output-format text` or `--sandbox` for `agy` runs, the CLI needs `--output-format text` for explicit text output and `--sandbox` to restrict terminal execution
- mismatch: launch.go:strippedEnv sets `HOME` and `TMPDIR` to an empty per-task scratch directory and strips `CODEX_HOME`, `ANTHROPIC_API_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`, and `OPENAI_API_KEY`, the CLIs need either the authenticated `HOME` (or `CODEX_HOME`) or an explicit API/OAuth token env var so they do not fail authentication (`claude` exits 1, `codex` exits 1 with 401, `agy` blocks 60s and exits 1)
- mismatch: launch.go:strippedEnv sets `PATH` without `$HOME/.local/bin`, the `claude` and `agy` binaries live in `$HOME/.local/bin` (with `claude` symlinked to `$HOME/.local/share/claude/versions/2.1.332`)
- mismatch: sandbox.go:seatbeltProfile uses `(deny default)` and only allows `file-write*` on `scratch`, `/dev/null`, and (when writable) `work` and Git metadata, the CLIs need `file-write*` on `$HOME/.claude` + `$HOME/.claude.json*` + `$HOME/Library/Caches/claude-cli-nodejs` (plus `/private/tmp` for Bash) for `claude`, `$HOME/.codex` (or `$CODEX_HOME`) for `codex`, and `$HOME/.gemini` for `agy`
- mismatch: sandbox.go:seatbeltProfile omits `mach-lookup` and `file-read*` on home config directories, macOS Keychains, `/opt/homebrew`, and `$HOME/.local/share/claude`, the CLIs need macOS DNS/TLS/Keychain Mach services and read access to their config and runtime directories to authenticate and start
- mismatch: sandbox.go:seatbeltProfile wraps `codex` in an outer `sandbox-exec` while `launch.go` leaves `codex`'s internal Seatbelt sandbox enabled (`--sandbox read-only` or default), macOS rejects nested `sandbox_apply` with `Operation not permitted` (exit 71) and breaks all `codex` tool execution
- mismatch: coding.go:Adapter.start creates an empty `.coding-scratch-*` directory and passes it as `HOME` into `buildCommand`, the CLIs need access to their authenticated home/config directories or injected credentials rather than an uninitialized `HOME`

## Separate state directories

Tested on macOS (`darwin`) on 2026-11-25 against the installed binaries (`claude 2.1.332`, `codex 0.171.0`, `agy 1.5.16`) using empty state directories under `.tmp/`, `</dev/null`, and `sandbox-exec` profiles denying read and write access to `$HOME/.claude`, `$HOME/.claude.json`, `$HOME/.codex`, `$HOME/.gemini`, and `$HOME/.agents`. Planned Sablecourt state roots: `$SABLECOURT_STATE_DIR/harness/claude`, `$SABLECOURT_STATE_DIR/harness/codex`, and `$SABLECOURT_STATE_DIR/harness/agy`.

### `claude`

| Field | Behavior |
| --- | --- |
| State relocation variable | `CLAUDE_CONFIG_DIR` (absolute path, e.g. `$SABLECOURT_STATE_DIR/harness/claude`) |
| What moves | `.claude.json` (user MCP servers and project state), `settings.json` (global settings and hooks), `CLAUDE.md`, `projects/<encoded-cwd>/memory/`, `skills/`, `agents/`, `plugins/`, `backups/`, `plans/`, and `sessions/`. None of the user's global `$HOME/.claude*` settings, hooks, MCP servers, or memory files load. |
| Headless error without login | Exit `1`; prints `Not logged in · Please run /login` after populating `.claude.json`, `backups/`, `plugins/store/`, `projects/<encoded-cwd>/memory/`, and `sessions/` inside `CLAUDE_CONFIG_DIR`. |
| Where the credential lives on macOS | macOS Keychain (`$HOME/Library/Keychains/login.keychain-db`), not a file inside `CLAUDE_CONFIG_DIR`. Default (unset `CLAUDE_CONFIG_DIR`) uses service `Claude Code-credentials` and account `<macos-user>`. With `CLAUDE_CONFIG_DIR` set, the service becomes `Claude Code-credentials-<sha256-8>` (first 8 hex characters of the SHA-256 of the normalized path) with account `<macos-user>`. Separate per directory: leaving real `HOME` intact did not pick up the existing `Claude Code-credentials` entry. |
| What still leaks from the user's setup | The credential still lives in the user's `login.keychain-db`, so a confined harness using subscription OAuth still needs Keychain access. Caches remain under `$HOME/Library/Caches/claude-cli-nodejs/` when `HOME` is unchanged, the binary resolves through `$HOME/.local/share/claude/`, and system managed settings (`/Library/Application Support/ClaudeCode/managed-settings.json`) plus workspace files (`./CLAUDE.md`, `./.claude/`, `./.mcp.json`) still apply. Denying `$HOME/.claude`, `$HOME/.claude.json`, and `$HOME/.agents` in `sandbox-exec` produced the same unauthenticated exit `1` without permission errors. |
| One-time login command | `mkdir -p "$SABLECOURT_STATE_DIR/harness/claude" && CLAUDE_CONFIG_DIR="$SABLECOURT_STATE_DIR/harness/claude" claude auth login` |

### `codex`

| Field | Behavior |
| --- | --- |
| State relocation variable | `CODEX_HOME` (absolute path, e.g. `$SABLECOURT_STATE_DIR/harness/codex`; the directory must exist beforehand or `codex` exits `1` with `Error finding codex home`) |
| What moves | `config.toml` (global settings and MCP servers), `auth.json`, `hooks.json`, `AGENTS.md` / `AGENTS.override.md`, SQLite state (`state_5.sqlite`, `logs_2.sqlite`, `goals_1.sqlite`, `memories_1.sqlite`, `queue_1.sqlite`), `installation_id`, `skills/`, `sessions/`, `shell_snapshots/`, `tmp/`, and `.tmp/plugins/`. None of the user's global `$HOME/.codex` settings, hooks, MCP servers, or memory files load. |
| Headless error without login | Exit `1`; populates the SQLite databases, `installation_id`, `skills/.system/`, and `.tmp/plugins/` inside `CODEX_HOME`, retries WebSocket and HTTPS 5 times each, and prints `ERROR: unexpected status 401 Unauthorized: Missing bearer or basic authentication in header, url: https://api.openai.com/v1/responses`. |
| Where the credential lives on macOS | File `$CODEX_HOME/auth.json` by default (`cli_auth_credentials_store = "file"` is the default; `security find-generic-password -s "Codex Auth"` returned not found on a logged-in setup). If `config.toml` sets `cli_auth_credentials_store = "keyring"` (or `"auto"` when keyring is active), it uses macOS Keychain service `Codex Auth` with an account key hashed from the canonicalized `CODEX_HOME` path. Separate per directory in both modes. |
| What still leaks from the user's setup | Nothing from `$HOME/.codex` or `$HOME/.agents`, and no macOS Keychain access is required under the default `file` store (denying `$HOME/.codex` and `$HOME/.agents` in `sandbox-exec` produced the same `401 Unauthorized` exit `1`). System-wide `/etc/codex/` (`hooks.json`, `skills/`) and workspace files (`.codex/config.toml`, `AGENTS.md`, `.env`) still apply. |
| One-time login command | `mkdir -p "$SABLECOURT_STATE_DIR/harness/codex" && CODEX_HOME="$SABLECOURT_STATE_DIR/harness/codex" codex login` |

### `agy`

| Field | Behavior |
| --- | --- |
| State relocation variable | `HOME` (e.g. `HOME="$SABLECOURT_STATE_DIR/harness/agy"`). `agy 1.5.16` has no dedicated config-directory variable: setting `AGY_HOME`, `ANTIGRAVITY_HOME`, `GEMINI_CLI_HOME`, and `XDG_CONFIG_HOME` with real `HOME` intact and denying `$HOME/.gemini` in `sandbox-exec` failed immediately (`Failed to start: open $HOME/.gemini/antigravity-cli/installation_id: operation not permitted`, exit `1`) and left the target directory empty. |
| What moves | `$HOME/.gemini/antigravity-cli/` (`settings.json`, hooks, `brain/`, `conversations/`, `conversation_summaries.db`, `builtin/`, `cache/`, `log/`, `installation_id`) and `$HOME/.gemini/config/` (`config.json`, `mcp_config.json`, `projects/`). None of the user's real `$HOME/.gemini` settings, hooks, MCP servers, or memory files load when `HOME` is overridden. |
| Headless error without login | Exit `1`; populates `$HOME/.gemini/antigravity-cli/` and `$HOME/.gemini/config/`, fails silent auth, prints an OAuth URL and `Waiting for authentication (timeout 60s)...`, ignores `--print-timeout 10s` during auth, and exits `1` after 60 seconds with `Error: authentication timed out.` and `error: authentication failed or timed out`. |
| Where the credential lives on macOS | Under real `HOME`, macOS Keychain (`$HOME/Library/Keychains/login.keychain-db`) via `zalando/go-keyring` (`/usr/bin/security`), under fixed service `gemini` and fixed account `antigravity`. That entry is shared across all `agy` and Antigravity surfaces for the macOS user and is not keyed by directory. Under a separate `HOME`, `security default-keychain` fails with `SecKeychainCopyDefault: A default keychain could not be found.`, so `agy` cannot read the user's `gemini`/`antigravity` entry; after a Keychain write failure, `agy` falls back to file-based token storage inside `$HOME/.gemini/`. |
| What still leaks from the user's setup | With `HOME` overridden, `$HOME/.gemini` and `$HOME/.agents` are isolated (verified under `sandbox-exec`), and the user's Keychain entry is not visible. The trade-off is that overriding `HOME` also moves `HOME` for every child process `agy` spawns (Git config, SSH config, caches), and macOS can show a "Keychain Not Found" dialog when `agy` first probes the missing default keychain before falling back to file storage. The binary itself still resolves from the real `$HOME/.local/bin/agy`, and workspace-local configs still apply. |
| One-time login command | `mkdir -p "$SABLECOURT_STATE_DIR/harness/agy" && HOME="$SABLECOURT_STATE_DIR/harness/agy" agy` (finish the OAuth flow, then run `/exit`; `agy` has no `login` subcommand) |

### Verdicts

- `claude`: isolation possible: partly (all config, hooks, MCP servers, and memory move into `CLAUDE_CONFIG_DIR` and the Keychain entry is hashed per directory, but on macOS the subscription token still lives in `$HOME/Library/Keychains/login.keychain-db` and caches remain under `$HOME/Library/Caches/claude-cli-nodejs/`)
- `codex`: isolation possible: yes (`CODEX_HOME` relocates all config, hooks, MCP servers, memory, skills, and SQLite state, and the default `file` credential store keeps the subscription token in `$CODEX_HOME/auth.json` without touching `$HOME/.codex`, `$HOME/.agents`, or macOS Keychain)
- `agy`: isolation possible: partly (`agy` has no config-directory env var, so isolation requires overriding `HOME`; that isolates `$HOME/.gemini/` and hides the shared `gemini`/`antigravity` Keychain entry, but breaks macOS default Keychain lookup, relies on file-storage fallback, and shifts `HOME` for all child processes)
