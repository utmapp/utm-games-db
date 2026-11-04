-- Which triage machine is working on a report. A machine takes the lease
-- before it starts a triage and renews it while the work is alive; another
-- machine skips a report whose lease has not expired, and takes over one
-- whose lease has (its machine stopped renewing). Release sets expires_at
-- to the release time, so the row keeps the last holder (for a reporter's
-- reply, which that machine's session should pick up).
CREATE TABLE triage_leases (
    report_id   INTEGER PRIMARY KEY REFERENCES reports(id) ON DELETE CASCADE,
    holder      TEXT NOT NULL,
    acquired_at INTEGER NOT NULL,
    expires_at  INTEGER NOT NULL
);
