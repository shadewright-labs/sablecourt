# Retry a failed task with a handoff

Re-run a task that ended, and optionally give the new run a summary of why the first one failed.

Prerequisites: the daemon is running and you are signed in ([install and start](install-and-start.md)); the task is in state `completed`, `failed` or `cancelled`.

## Steps

1. Find the task and check its state:

   ```sh
   sablectl task list
   sablectl task inspect <task-id>
   ```

2. Retry it as it was:

   ```sh
   sablectl task retry <task-id>
   ```

3. Or retry it with the failure handoff, which adds the reason, the last events, the captured output and the last messages to the new task's context:

   ```sh
   sablectl task retry <task-id> --handoff
   ```

   In the web UI the task page shows "Retry" and "Fix from failure (handoff)" buttons for a task that is not running. The API is `POST /api/v1/tasks/{id}/retry`, with `?handoff=true` for the handoff.

## What the retry does

- It submits a new task for the same workload with the original input, `retried_from` set to the old task and `retry_count` one higher.
- The idempotency key is `retry-<old task id>-<attempt>`, new for each attempt. Retrying the same task a second time returns the task the first retry created instead of starting another. To go again, retry the new task.
- With a handoff, the input also carries the handoff block, which the new task's [context packet](../how-it-works/context-and-learning.md) shows under "Failure Handoff". It is redacted and bounded.
- The API and `sablectl` write an audit row `task.retry` with outcome `submitted`, naming the old task and whether a handoff was attached. The web UI button does not write that row.
- A task that is still running or whose runtime cleanup is pending is refused (HTTP 400, text "retry requires a terminal task" or "runtime cleanup is pending"). A council round task cannot be retried by hand ("council retries require a planned reservation").

## Retry without you

A workflow step with `maxRetries` retries itself when its run fails, with a handoff, until `retry_count` reaches the limit. A verifier that answers `VERDICT: CHANGES_REQUESTED` triggers the same loop on the author step, with the verifier's findings as the handoff. Each automatic retry writes `task.auto_remediate` (after a failure) or `workflow.verify` (after a verdict). See [verifier steps](../how-it-works/context-and-learning.md#verifier-steps).

## Check it worked

```sh
sablectl task inspect <new-task-id>
sablectl audit tail
```

The new task starts in `submitted`. After a retry through `sablectl` or the API the audit shows `task.retry`. If the retry was refused, the error text names the reason; [find why refused](find-why-refused.md) lists where else a refusal is recorded.
