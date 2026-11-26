-- Before approvals were single-use, executing an approved action left its row
-- in status 'approved', and no use was recorded. No row approved before this
-- migration can be told apart from one already used, so each one is expired:
-- an approval granted before the upgrade cannot authorize an action after it.
-- The update scans the approvals table without an index on status. The table
-- holds one row per approval request (thousands over a long
-- deployment, not millions), and the migration runs once before the daemon
-- serves, so the scan and its write lock last milliseconds. An index would not
-- help here: building it scans the table too.
UPDATE approvals SET status = 'expired' WHERE status = 'approved';
