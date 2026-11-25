# Verify the audit chain

Check that the rows in the audit log link up, export them, and keep a checkpoint so you can notice if the newest rows are cut off later.

Prerequisites: the daemon is running and `sablectl` can reach it with the operator credential (see [identity and access](../how-it-works/identity-and-access.md)).

## Steps

1. Run the check.

   ```sh
   sablectl audit verify
   ```

   On success it prints `audit hash chain valid (N rows verified)`. With `--json` it prints `{"valid":true,"verifiedRows":N}`.

2. If the chain is broken, the command exits with an error that carries HTTP status 409 and a body such as `{"valid":false,"verifiedRows":120,"error":"audit chain broken at row 121: ..."}`. The row ID in the message is the first row that fails; `verifiedRows` counts the rows before it. Export the log before you touch anything (step 3).

3. Export the log as JSON lines, oldest first.

   ```sh
   sablectl audit export > audit.jsonl
   ```

   The export holds at most the newest 10000 rows. Each line has `ID`, `Category`, `TaskID`, `WorkloadName`, `Action`, `Outcome`, `DetailJSON`, `PrevHash`, `RowHash` and `CreatedAt`.

4. Save a checkpoint outside the database. Take the newest row and store its ID and `RowHash` somewhere the daemon's host cannot rewrite, such as a note in a repository or another machine.

   ```sh
   sablectl audit tail --json
   ```

   The first entry of the list is the newest row.

5. Next time, after `sablectl audit verify` passes, check the checkpoint: export again and confirm that a row with the saved ID exists and has the saved `RowHash`. If the row is missing or differs, rows were removed or rewritten after your checkpoint, which verification alone would not have told you.

## Check it worked

`sablectl doctor` shows the same check on its audit line (`valid`, with the row count). The Audit page of the web UI verifies the chain each time you open it and shows the result above the table.

## What a pass does not prove

A pass means the rows present link up and match their hashes. It does not detect deletion of the newest rows, a row whose hash was emptied (the check skips it), or a rewrite of the whole chain by someone who can write the database, because the hash has no key. It also says nothing about events that are never recorded, such as sign-ins. `sablectl doctor` prints no audit line at all when the chain is broken, so use `sablectl audit verify` to see the failure. The full list is in [audit](../how-it-works/audit.md); the checkpoint in step 4 is a manual workaround for the first gap, not a feature of the product.

## In depth

- [Audit](../how-it-works/audit.md)
- [Threat model](../threat-model.md)
