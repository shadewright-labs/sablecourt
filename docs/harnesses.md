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
