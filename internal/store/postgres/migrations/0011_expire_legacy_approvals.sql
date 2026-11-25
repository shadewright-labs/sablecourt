-- Before approvals were single-use, executing an approved action left its row
-- in status 'approved', and no use was recorded. No row approved before this
-- migration can be told apart from one already used, so each one is expired:
-- an approval granted before the upgrade cannot authorize an action after it.
UPDATE approvals SET status = 'expired' WHERE status = 'approved';
