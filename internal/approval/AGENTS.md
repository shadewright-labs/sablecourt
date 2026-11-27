This file adds to the root AGENTS.md and does not repeat it.

# internal/approval

This package queues, approves and denies approvals. It also covers the gate's approval path. The gate consumes an approval in `internal/gate/mcp/gate.go`, which calls `ConsumeApproval` on the store (interface in `internal/store/store.go`, implemented in `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`).

- An approval for a typed action (`RegisterTypedAction` in `internal/gate/mcp/gate.go`) is bound to the digest of the decoded typed value. Untyped payloads (`RegisterAction`, MCP tool approvals) are hashed as untyped JSON. A different value needs a new approval.
- An approval is used once. It moves from approved to consumed before the side effect runs, so a failed action needs a fresh approval.
- Expiry is checked twice: when an approver approves, on the daemon clock (`Queue.Approve` in `approval.go`, `ApproveWithSignature` in `device.go`), and again at the moment of use, on the database clock inside `ConsumeApproval`. The check at use is the one that decides.
- Only `ConsumeApproval` consumes an approval. Add no other path that sets the consumed status.
- An empty digest is refused (`ErrDigestRequired` in `approval.go`). Never default it from the stored value.
- Model output never authorizes (invariant 1, ADR-0005). Execution is authorized by a deterministic policy `allow` rule or by an out-of-band human approval, through an approver (`Queue.Approve`) or a paired device signature (`Queue.ApproveWithSignature`).
- Tests: `internal/approval/refusal_test.go` and `internal/store/sqlite/approval_consume_test.go`. Add a refusal row for each new refusal path.
