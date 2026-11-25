# Review an approval

Look at a pending approval, check what it pins, and approve or deny it. Approving needs a person at the machine that runs the daemon.

## Before you start

- The daemon runs and you can sign in (`sablectl` reads the operator token; the web UI uses the login page).
- The daemon host is macOS. On Linux there is no approver, so approving fails and you can only deny.
- A task is in state `input-required` because a workload called an action whose policy effect is `approve`.

## Steps

1. List pending approvals:

   ```sh
   sablectl approval list
   ```

   The table shows the approval ID, workload, capability, the first 16 characters of the SHA-256 digest and the expiry time (UTC). An approval lives 15 minutes.

2. Read the full payload and digest as JSON:

   ```sh
   sablectl approval list --json
   ```

   Check `Capability`, `WorkloadName`, and `PayloadCanonicalJSON`. The dialog shows only the first 220 characters of that payload, so read long fields here.

3. Approve, pinning the digest you read:

   ```sh
   sablectl approval approve <approval-id> --digest <full sha256>
   ```

   A dialog titled "Sablecourt Approval" opens on the daemon host. Click Approve within 60 seconds. The default button is Deny. The command waits up to 65 seconds.

4. Or deny:

   ```sh
   sablectl approval deny <approval-id> --reason "wrong branch"
   ```

   Denying needs no dialog.

The web UI does the same from the dashboard: each pending approval has Approve and Deny buttons, and the Approve button sends the stored digest.

## Check it worked

- `sablectl approval list` no longer shows the approval.
- `sablectl task inspect <task-id>` shows the task back in `working` (event `approval_decided`) within a few seconds, and then the workload repeats its call.
- `sablectl audit tail` shows an `approval` row with outcome `approved` or `denied`.

## If it fails

| Message | Meaning |
| --- | --- |
| `approval payload digest mismatch` | The digest you passed is not the stored one. List again. |
| `approval expired` | 15 minutes passed. The workload must ask again. |
| `out-of-band confirmation denied or cancelled` | You clicked Deny, closed the dialog, or 60 seconds passed. |
| `conflict` | The approval is no longer pending. |
| `no out-of-band approver configured` | The daemon runs without one (Citadel mode). |

An approved action can be repeated by the same task until its lease ends; approving once is permission for that exact payload, not for one run. See [approvals](../how-it-works/approvals.md#not-built-yet).

## Related

- [Approvals](../how-it-works/approvals.md), [tools and actions](../how-it-works/tools-and-actions.md)
