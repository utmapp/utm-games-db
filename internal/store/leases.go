package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TriageLease says which triage machine works on a report, until when.
type TriageLease struct {
	ReportID   int64     `json:"report_id"`
	Holder     string    `json:"holder"`
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Active     bool      `json:"active"` // not expired (or released) at the time it was read
}

// ErrAlreadyResolved is returned for a write that finishes something
// another caller (another triage machine) already finished: duplicate work,
// safe to ignore.
var ErrAlreadyResolved = errors.New("already resolved")

// ErrLeaseHeld is returned when another holder has an active lease.
var ErrLeaseHeld = errors.New("the report's triage lease is held by another machine")

// LeaseHeldError carries the lease another holder has.
type LeaseHeldError struct{ Lease TriageLease }

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("report %d is being triaged by %q (lease until %s)", e.Lease.ReportID, e.Lease.Holder,
		e.Lease.ExpiresAt.UTC().Format(time.RFC3339))
}

func (e *LeaseHeldError) Unwrap() error { return ErrLeaseHeld }

const leaseCols = `report_id, holder, acquired_at, expires_at`

func scanLease(row interface{ Scan(...any) error }, now time.Time) (TriageLease, error) {
	var l TriageLease
	var acq, exp int64
	if err := row.Scan(&l.ReportID, &l.Holder, &acq, &exp); err != nil {
		return l, err
	}
	l.AcquiredAt, l.ExpiresAt = time.Unix(acq, 0).UTC(), time.Unix(exp, 0).UTC()
	l.Active = exp > now.Unix()
	return l, nil
}

// AcquireTriageLease takes or renews the lease on a report for holder, for
// ttl from now. It succeeds when there is no lease, when the lease expired,
// or when holder already has it (a renewal keeps acquired_at); otherwise it
// returns a *LeaseHeldError with the current lease. One statement, so two
// machines racing for the same report cannot both win.
func (s *Store) AcquireTriageLease(ctx context.Context, reportID int64, holder string, ttl time.Duration) (TriageLease, error) {
	holder = strings.TrimSpace(holder)
	if holder == "" || len(holder) > 200 {
		return TriageLease{}, errors.New("holder must be 1–200 characters")
	}
	if ttl < time.Minute || ttl > 6*time.Hour {
		return TriageLease{}, errors.New("ttl must be between 60 s and 6 h")
	}
	if err := s.reportExists(ctx, reportID); err != nil {
		return TriageLease{}, err
	}
	now := time.Now()
	res, err := s.DB.Writer.ExecContext(ctx, `INSERT INTO triage_leases (report_id, holder, acquired_at, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (report_id) DO UPDATE SET
			acquired_at = CASE WHEN triage_leases.holder = excluded.holder AND triage_leases.expires_at > ?
			                   THEN triage_leases.acquired_at ELSE excluded.acquired_at END,
			holder = excluded.holder,
			expires_at = excluded.expires_at
		WHERE triage_leases.holder = excluded.holder OR triage_leases.expires_at <= ?`,
		reportID, holder, now.Unix(), now.Add(ttl).Unix(), now.Unix(), now.Unix())
	if err != nil {
		return TriageLease{}, err
	}
	n, _ := res.RowsAffected()
	l, err := s.GetTriageLease(ctx, reportID)
	if err != nil {
		return TriageLease{}, err
	}
	if n == 0 {
		return l, &LeaseHeldError{Lease: l}
	}
	return l, nil
}

// reportExists is nil when the report is there, else ErrNotFound.
func (s *Store) reportExists(ctx context.Context, reportID int64) error {
	var exists int
	if err := s.DB.Reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM reports WHERE id = ?`, reportID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrNotFound
	}
	return nil
}

// ReleaseTriageLease ends holder's lease on a report now (the row stays, with
// its holder, as the last machine that worked on it). Releasing a lease that
// is already expired or released, or a report that never had one, is not an
// error; releasing another holder's active lease is a *LeaseHeldError, and
// a report that does not exist is ErrNotFound.
func (s *Store) ReleaseTriageLease(ctx context.Context, reportID int64, holder string) error {
	holder = strings.TrimSpace(holder)
	if holder == "" {
		return errors.New("holder must not be empty")
	}
	now := time.Now().Unix()
	res, err := s.DB.Writer.ExecContext(ctx, `UPDATE triage_leases SET expires_at = ? WHERE report_id = ? AND holder = ? AND expires_at > ?`,
		now, reportID, holder, now)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	l, err := s.GetTriageLease(ctx, reportID)
	if errors.Is(err, ErrNotFound) {
		return s.reportExists(ctx, reportID)
	}
	if err != nil {
		return err
	}
	if l.Active && l.Holder != holder {
		return &LeaseHeldError{Lease: l}
	}
	return nil
}

// GetTriageLease is the report's lease row (active or not), or ErrNotFound.
// It reads from the reader pool (a committed write is visible to the next
// read in WAL mode), so it never queues behind a long write transaction.
func (s *Store) GetTriageLease(ctx context.Context, reportID int64) (TriageLease, error) {
	l, err := scanLease(s.DB.Reader.QueryRowContext(ctx, `SELECT `+leaseCols+` FROM triage_leases WHERE report_id = ?`, reportID), time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	return l, err
}

// CheckTriageWriter returns a *LeaseHeldError when another holder than
// holder ("" for a caller that names none) has an active lease on the
// report: its triage writes would collide with that machine's.
func (s *Store) CheckTriageWriter(ctx context.Context, reportID int64, holder string) error {
	l, err := s.GetTriageLease(ctx, reportID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.Active && l.Holder != strings.TrimSpace(holder) {
		return &LeaseHeldError{Lease: l}
	}
	return nil
}

// TriageLeases are the lease rows for the given reports, by report id
// (empty for no ids).
func (s *Store) TriageLeases(ctx context.Context, ids []int64) (map[int64]TriageLease, error) {
	out := map[int64]TriageLease{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT `+leaseCols+` FROM triage_leases WHERE report_id IN (`+
		strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		l, err := scanLease(rows, now)
		if err != nil {
			return nil, err
		}
		out[l.ReportID] = l
	}
	return out, rows.Err()
}

// StaleTriage are the reports in progress whose lease expired without a
// release: the machine working on them stopped renewing. (A report in
// progress with no lease row at all is someone triaging by hand, and is
// left alone.)
func (s *Store) StaleTriage(ctx context.Context) ([]TriageLease, error) {
	now := time.Now()
	rows, err := s.DB.Reader.QueryContext(ctx, `SELECT l.report_id, l.holder, l.acquired_at, l.expires_at FROM triage_leases l
		JOIN reports r ON r.id = l.report_id
		WHERE r.triage_status = 'in_progress' AND r.visibility = 'visible' AND l.expires_at <= ?
		ORDER BY l.expires_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TriageLease
	for rows.Next() {
		l, err := scanLease(rows, now)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ForgetInactiveTriageLease deletes the report's lease row when it is not
// active, so a report put in progress by hand (no lease) is not mistaken for
// one a machine abandoned.
func (s *Store) ForgetInactiveTriageLease(ctx context.Context, reportID int64) error {
	_, err := s.DB.Writer.ExecContext(ctx, `DELETE FROM triage_leases WHERE report_id = ? AND expires_at <= ?`, reportID, time.Now().Unix())
	return err
}
