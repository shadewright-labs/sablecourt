# Watch and steer

Status: built, with Takeover parked by [ADR-0011](../adr/0011-sanctum-scope-trust-and-cli-harnesses.md).

Watch a running task, open a Conclave from a manifest, steer it with a Decree, share a Relic and close it.

## Prerequisites

- A running daemon and a `Conclave` manifest that is loaded ([write manifests](write-manifests.md)).

## Steps

1. Open the room, optionally with a title and a first Decree. `always-on` Vassals start now:

   ```sh
   sablectl conclave open review-room --title "Parser review" --decree "Review the parser change"
   ```

2. Watch messages arrive, or list the rooms and inspect one with its Relics:

   ```sh
   sablectl conclave watch <id>
   sablectl conclave list
   sablectl conclave inspect <id>
   ```

3. Steer with a Decree. `--to` takes a role or `all` (default). A Decree also wakes `on-mention` Vassals it addresses, and resumes a room a Ward paused:

   ```sh
   sablectl conclave decree <id> "Focus on error handling" --to reviewer
   ```

4. Share a file as a Relic, or read one. `put` reads standard input when `--body` is omitted:

   ```sh
   sablectl conclave relic put <id> notes.md --type text/markdown < notes.md
   sablectl conclave relic get <id> notes.md
   ```

5. Watch the live output of a host-run coding task (only with `--unsafe-host-harnesses`, unsupported) or a long-lived container workload. It is read-only and redacted. Council turns in containers have no live stream:

   ```sh
   sablectl task scry <task-id>
   ```

6. Close the room. The CLI then cancels the room's tasks that are still active:

   ```sh
   sablectl conclave close <id>
   ```

## Check it worked

`conclave inspect` shows `state=` (`active`, `input-required`, `completed` or `cancelled`) and, for a paused room, `ward: <reason>`. After a Decree resumed it, the state is `active` again. A room that stopped on its wall time cannot be resumed.

## In depth

[Conclaves](../how-it-works/conclaves.md), [council](../how-it-works/council.md).
