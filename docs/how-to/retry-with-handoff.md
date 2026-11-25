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

   In the web UI the task page shows "Retry" and "Fix from failure (handoff)" buttons for any task that is not submitted, working or input-required. A suspended task shows them too, and the daemon refuses it. The API is `POST /api/v1/tasks/{id}/retry`, with `?handoff=true` for the handoff.

## What the retry does

- It submits a new task for the same workload with the input of the old task, `retried_from` set to the old task and `retry_count` one higher. The input is decoded and encoded again, so key order can change and a JSON integer too large for a 64-bit float can lose precision. An input that is not a JSON object becomes the `prompt` field.
- For a task outside a Conclave, the idempotency key is `retry-<old task id>-<attempt>`, new for each attempt. For a task in a Conclave it is `conclave-<conclave id>-retry-<old task id>-<attempt>`. Retrying the same task a second time returns the task the first retry created instead of starting another. To go again, retry the new task.
  The key does not include the handoff choice, so if the task was already retried without a handoff, a retry with `--handoff` returns that existing task, without the handoff block, and the API still answers 201 and tries to write `task.retry` with `handoff` true (the API ignores a failed audit write). To get a handoff, wait until the existing retry has ended, then retry that task with the handoff. The same holds the other way round.
- With a handoff, the input also carries the handoff block. It is redacted, and each part has a size cut (the last 6 events, the last 4096 bytes of each artifact, the last 5 messages of 1024 bytes each). The new task's [context packet](../how-it-works/context-and-learning.md) shows the block under "Failure Handoff". A task of a deterministic-controller workload gets no packet, so it receives the block only in its input.
- For a workflow step on a `coding-agent` workload, the retry starts from the commit the failed attempt recorded. If none was recorded, the new task fails at launch with "runtime cannot guarantee the author's checkout for this step".
- The API and `sablectl` write an audit row `task.retry` with outcome `submitted`, naming the old task and whether a handoff was attached. The web UI button does not write that row.
- A task that is still running or whose runtime cleanup is pending is refused (HTTP 400, text "retry requires a terminal task" or "runtime cleanup is pending"). A council round task cannot be retried by hand ("council retries require a planned reservation").

## Retry without you

A task of a workflow step with `maxRetries`, or whose input sets `auto_remediate`, is retried with a handoff when it fails, until `retry_count` reaches the limit. This happens only if the daemon saw the runtime exit; a task that failed to launch or was lost on restart is not retried. A verifier that answers `VERDICT: CHANGES_REQUESTED` starts the author step again, with the verifier's findings as the handoff, while `retry_count` is below the limit. A retry after a failure writes `task.auto_remediate`. A `CHANGES_REQUESTED` verdict writes `workflow.verify` with outcome `changes_requested`, whether or not another run follows. See [verifier steps](../how-it-works/context-and-learning.md#verifier-steps).

## Check it worked

```sh
sablectl task inspect <new-task-id>
sablectl audit tail
```

The new task starts in `submitted`. After a retry through `sablectl` or the API the audit shows `task.retry`. If the retry was refused, the error text names the reason; [find why refused](find-why-refused.md) lists where else a refusal is recorded.
