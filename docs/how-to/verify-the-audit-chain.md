# Verify the audit chain

Check that the hashed rows in the audit log link up, export them, and keep a checkpoint so you can notice later if rows up to it were cut off or rewritten.

Prerequisites: the daemon is running and `sablectl` can reach it with the operator credential (see [identity and access](../how-it-works/identity-and-access.md)).

## Steps

1. Run the check.

   ```sh
   sablectl audit verify
   ```

   On success it prints `audit hash chain valid (N rows verified)`, where N counts the rows that have a hash. With `--json` it prints `{"valid":true,"verifiedRows":N}`.

2. If the check fails, the command exits with an error that carries HTTP status 409 and a body such as `{"valid":false,"verifiedRows":120,"error":"audit chain broken at row 121: ..."}`. For a broken link or a changed row the message names the first row that failed; a database error is reported as that error instead. `verifiedRows` counts the hashed rows checked before the failure. Export the log before you touch anything (step 3).

3. Export the log as JSON lines, oldest first.

   ```sh
   sablectl audit export > audit.jsonl
   ```

   The export holds at most the newest 10000 rows. Each line has `ID`, `Category`, `TaskID`, `WorkloadName`, `Action`, `Outcome`, `DetailJSON`, `PrevHash`, `RowHash` and `CreatedAt`.

4. Save a checkpoint outside the database. Take the newest row and store its ID and `RowHash` somewhere the daemon's host cannot rewrite, such as a note in a repository or on another machine.

   ```sh
   sablectl audit tail --json
   ```

   The first entry of the list is the newest row.

5. Next time, after `sablectl audit verify` passes, check the checkpoint: export again and look for a row with the saved ID and the saved `RowHash`. If it is missing or its `RowHash` differs, rows up to the checkpoint were removed, renumbered or rewritten, or more than 10000 rows have been added since, which pushes the checkpoint out of the export. A matching row says nothing about rows added after it.

## Check it worked

When the check passes, `sablectl doctor` shows it on its audit line (`valid`, with the row count). The Audit page of the web UI runs the check each time you open it and shows the result above the table.

## What a pass does not prove

A pass means the hashed rows present link up and match their fields. It does not detect deletion of the newest rows, a row whose hash was emptied (the check skips it), renumbered rows (the ID is not hashed), or a rewrite of the whole chain by someone who can write the database, because the hash has no key. It says nothing about events that are never recorded, such as sign-ins. `sablectl doctor` prints no audit line at all when the check fails, so use `sablectl audit verify` to see the failure. The full list is in [audit](../how-it-works/audit.md); the checkpoint in step 4 is a manual workaround for the first gap, not a feature of the product.

## In depth

- [Audit](../how-it-works/audit.md)
- [Threat model](../threat-model.md)
