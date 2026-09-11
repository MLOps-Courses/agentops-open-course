package data

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

const (
	// actionRestartService and actionResolveIncident name the two guarded
	// mutations. They are two thirds of the idempotency key, so their spelling
	// is a stored contract rather than a label: changing either one makes every
	// past write unrecognizable as a replay.
	actionRestartService  = "restart_service"
	actionResolveIncident = "resolve_incident"

	// The dataset is a simulation, and the audit detail says so out loud. An
	// auditor reading the trail must never be left wondering whether a real
	// service was restarted.
	restartDetail = "service restarted and marked operational (mock)"
	resolveDetail = "incident resolved (mock)"
)

// AuditIdentity is the who and the why of one guarded write.
//
// It is a struct rather than five string parameters because Python made every
// one of them keyword-only, and five positional strings at a call site is a
// transposition waiting to happen — one that would attribute an action to the
// wrong person in an append-only trail.
type AuditIdentity struct {
	// Actor is the agent or component that performed the action.
	Actor string
	// ApprovedBy is the human who authorized it. This is the field the
	// human-in-the-loop confirmation exists to fill.
	ApprovedBy string
	// Rationale is the justification recorded alongside the action.
	Rationale string
	// SessionID and InvocationID tie the row back to the conversation and the
	// single model turn that produced it. InvocationID is part of the
	// idempotency key; SessionID deliberately is not, so the same logical write
	// redelivered under a new session is still recognized as a replay.
	SessionID    string
	InvocationID string
}

// AuditRequest is one full audit append: who and why, plus what.
type AuditRequest struct {
	AuditIdentity
	// ContextSummary is the decision context read under the write lock.
	ContextSummary string
	// Action and Target complete the idempotency key.
	Action string
	Target string
	// Detail is the human-readable outcome.
	Detail string
}

// inWriteTransaction runs fn under one BEGIN IMMEDIATE.
//
// fn reports whether its work should be committed. A replay and a no-op both
// finish by *rolling back*: they read state under the write lock and changed
// nothing, and rolling back releases the lock without claiming otherwise.
//
// The write lock is taken up front, before any read, which is the entire point
// of IMMEDIATE. A deferred transaction would read the service status, then
// upgrade to a write lock, and the row could have changed in between — the
// audit trail would then describe a state that never preceded the write.
func (d *database) inWriteTransaction(ctx context.Context, fn func(tx *sql.Tx) (bool, error)) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("take the write lock: %w", err)
	}
	commit, err := fn(tx)
	if err != nil || !commit {
		// The rollback error is dropped deliberately: it can only report a
		// transaction that has already finished, and surfacing it would displace
		// the refusal the caller actually needs to read.
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit the write: %w", err)
	}
	return nil
}

// AppendAudit appends one audit entry, or returns the row a replayed logical
// write already produced.
//
// It imposes no restriction on Action or Target: this is the general append
// that other layers use to record their own guarded operations.
func (s *Store) AppendAudit(ctx context.Context, request AuditRequest) (domain.AuditEntry, error) {
	db, err := s.openForWrite(ctx)
	if err != nil {
		return domain.AuditEntry{}, err
	}
	var entry domain.AuditEntry
	txErr := db.inWriteTransaction(ctx, func(tx *sql.Tx) (bool, error) {
		existing, err := auditForIdempotencyKey(ctx, tx, request.InvocationID, request.Action, request.Target)
		if err != nil {
			return false, err
		}
		if existing != nil {
			// The first row is the record of what happened. A redelivery
			// carrying a different rationale must not rewrite it — that is the
			// difference between an audit trail and a cache.
			entry = *existing
			return false, nil
		}
		appended, err := appendAudit(ctx, tx, request)
		if err != nil {
			return false, err
		}
		entry = appended
		return true, nil
	})
	if err := db.closeWith(db.fail(txErr)); err != nil {
		return domain.AuditEntry{}, err
	}
	return entry, nil
}

// RestartServiceWithAudit restarts a service and audits it once, atomically.
//
// It returns nil, nil when there is no such service — the honest "nothing to
// do", which is where Python returned None. A replay returns the original entry
// without re-applying the mutation, so a later state change survives
// redelivery.
func (s *Store) RestartServiceWithAudit(
	ctx context.Context, name domain.Slug, identity AuditIdentity,
) (*domain.AuditEntry, error) {
	db, err := s.openForWrite(ctx)
	if err != nil {
		return nil, err
	}
	var entry *domain.AuditEntry
	txErr := db.inWriteTransaction(ctx, func(tx *sql.Tx) (bool, error) {
		existing, err := auditForIdempotencyKey(ctx, tx, identity.InvocationID, actionRestartService, string(name))
		if err != nil {
			return false, err
		}
		if existing != nil {
			entry = existing
			return false, nil
		}
		contextSummary, found, err := restartContext(ctx, tx, name)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		if s.onRestartContext != nil {
			s.onRestartContext()
		}
		result, err := tx.ExecContext(ctx, "UPDATE services SET status = 'operational' WHERE name = ?", string(name))
		if err != nil {
			return false, fmt.Errorf("restart service %s: %w", name, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("count the restarted services: %w", err)
		}
		if affected == 0 {
			// The row vanished between the context read and the update. Nothing
			// was changed, so nothing is audited.
			return false, nil
		}
		appended, err := appendAudit(ctx, tx, AuditRequest{
			AuditIdentity:  identity,
			ContextSummary: contextSummary,
			Action:         actionRestartService,
			Target:         string(name),
			Detail:         restartDetail,
		})
		if err != nil {
			return false, err
		}
		entry = &appended
		return true, nil
	})
	if err := db.closeWith(db.fail(txErr)); err != nil {
		return nil, err
	}
	return entry, nil
}

// ResolveIncidentWithAudit resolves an incident and audits it once, atomically.
//
// It returns nil, nil for an unknown incident and for one that is already
// resolved. The second case matters: resolving twice is not an error, it is a
// no-op, and a fresh invocation over an already-resolved incident must not
// append a second audit row claiming otherwise.
func (s *Store) ResolveIncidentWithAudit(
	ctx context.Context, incidentID domain.IncidentID, identity AuditIdentity,
) (*domain.AuditEntry, error) {
	db, err := s.openForWrite(ctx)
	if err != nil {
		return nil, err
	}
	var entry *domain.AuditEntry
	txErr := db.inWriteTransaction(ctx, func(tx *sql.Tx) (bool, error) {
		existing, err := auditForIdempotencyKey(ctx, tx, identity.InvocationID, actionResolveIncident, string(incidentID))
		if err != nil {
			return false, err
		}
		if existing != nil {
			entry = existing
			return false, nil
		}
		contextSummary, found, err := resolutionContext(ctx, tx, incidentID)
		if err != nil {
			return false, err
		}
		if !found {
			return false, nil
		}
		// The status guard is repeated in the UPDATE, not merely checked above,
		// so the write itself is the thing that refuses to resolve twice.
		result, err := tx.ExecContext(ctx,
			"UPDATE incidents SET status = 'resolved', resolved_at = ? WHERE id = ? AND status != 'resolved'",
			utcNow(), string(incidentID),
		)
		if err != nil {
			return false, fmt.Errorf("resolve incident %s: %w", incidentID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("count the resolved incidents: %w", err)
		}
		if affected == 0 {
			return false, nil
		}
		appended, err := appendAudit(ctx, tx, AuditRequest{
			AuditIdentity:  identity,
			ContextSummary: contextSummary,
			Action:         actionResolveIncident,
			Target:         string(incidentID),
			Detail:         resolveDetail,
		})
		if err != nil {
			return false, err
		}
		entry = &appended
		return true, nil
	})
	if err := db.closeWith(db.fail(txErr)); err != nil {
		return nil, err
	}
	return entry, nil
}

// restartContext reads the decision context for a restart, and reports whether
// the service exists at all.
//
// It runs inside the write transaction on purpose. The summary has to describe
// the rows the write is about to change, not a snapshot another writer has
// since moved past, or the audit trail records a justification for something
// that did not happen.
func restartContext(ctx context.Context, q querier, name domain.Slug) (string, bool, error) {
	var status string
	err := q.QueryRowContext(ctx, "SELECT status FROM services WHERE name = ?", string(name)).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read the status of service %s: %w", name, err)
	}

	rows, err := q.QueryContext(ctx, `
		SELECT id
		FROM incidents
		WHERE service = ? AND status != 'resolved'
		ORDER BY opened_at DESC`, string(name),
	)
	if err != nil {
		return "", false, fmt.Errorf("list the open incidents of service %s: %w", name, err)
	}
	defer func() { _ = rows.Close() }()
	var incidentIDs []string
	for rows.Next() {
		var incidentID string
		if err := rows.Scan(&incidentID); err != nil {
			return "", false, fmt.Errorf("scan the open incidents of service %s: %w", name, err)
		}
		incidentIDs = append(incidentIDs, incidentID)
	}
	if err := rows.Err(); err != nil {
		return "", false, fmt.Errorf("read the open incidents of service %s: %w", name, err)
	}

	open := "none"
	if len(incidentIDs) > 0 {
		open = strings.Join(incidentIDs, ", ")
	}
	return fmt.Sprintf("service %s was %s; open incidents: %s", name, status, open), true, nil
}

// resolutionContext reads the decision context for a resolution, and reports
// whether the incident is resolvable at all — an unknown incident and an
// already-resolved one are both "no".
func resolutionContext(ctx context.Context, q querier, incidentID domain.IncidentID) (string, bool, error) {
	var service, title, severity, status, runbook string
	err := q.QueryRowContext(ctx, `
		SELECT service, title, severity, status, runbook
		FROM incidents
		WHERE id = ?`, string(incidentID),
	).Scan(&service, &title, &severity, &status, &runbook)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read incident %s: %w", incidentID, err)
	}
	if status == string(domain.IncidentStatusResolved) {
		return "", false, nil
	}
	return fmt.Sprintf("incident %s (%s, %s) on %s: %s; runbook %s",
		incidentID, severity, status, service, title, runbook), true, nil
}

// auditForIdempotencyKey returns the first audit row for one logical write, or
// nil if it has not run.
//
// The key is exactly (invocation_id, action, target), the same three columns
// the unique index enforces, so this lookup and the constraint can never
// disagree about what counts as a replay.
func auditForIdempotencyKey(
	ctx context.Context, q querier, invocationID, action, target string,
) (*domain.AuditEntry, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT *
		FROM audit_log
		WHERE invocation_id = ? AND action = ? AND target = ?
		ORDER BY id
		LIMIT 1`, invocationID, action, target,
	)
	if err != nil {
		return nil, fmt.Errorf("look up the audit idempotency key: %w", err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read the audit_log columns: %w", err)
	}
	if !rows.Next() {
		if rowsErr := rows.Err(); rowsErr != nil {
			return nil, fmt.Errorf("read the audit idempotency key: %w", rowsErr)
		}
		return nil, nil
	}
	row, err := scanAuditRow(rows, columns)
	if err != nil {
		return nil, err
	}
	entry, err := domain.NewAuditEntry(row)
	if err != nil {
		return nil, fmt.Errorf("%w: audit entry %d is not valid: %w", ErrDataAccess, row.ID, err)
	}
	return &entry, nil
}

// appendAudit appends one audit row using the caller's transaction.
//
// It stamps the schema version this binary writes and returns the entry it
// built rather than re-reading the row: the values are the ones just inserted,
// and a second read would only add a chance for them to differ.
func appendAudit(ctx context.Context, tx execer, request AuditRequest) (domain.AuditEntry, error) {
	timestamp := utcNow()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO audit_log
			(schema_version, ts, actor, approved_by, rationale, context_summary,
			 session_id, invocation_id, action, target, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		domain.CurrentAuditSchemaVersion,
		timestamp,
		request.Actor,
		request.ApprovedBy,
		request.Rationale,
		request.ContextSummary,
		request.SessionID,
		request.InvocationID,
		request.Action,
		request.Target,
		request.Detail,
	)
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("append the audit entry: %w", err)
	}
	entryID, err := result.LastInsertId()
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("%w: SQLite did not return an audit entry id: %w", ErrDataAccess, err)
	}
	entry, err := domain.NewAuditEntry(domain.AuditEntryRow{
		ID:             entryID,
		SchemaVersion:  domain.CurrentAuditSchemaVersion,
		Ts:             timestamp,
		Actor:          request.Actor,
		ApprovedBy:     request.ApprovedBy,
		Rationale:      request.Rationale,
		ContextSummary: request.ContextSummary,
		SessionID:      request.SessionID,
		InvocationID:   request.InvocationID,
		Action:         request.Action,
		Target:         request.Target,
		Detail:         request.Detail,
	})
	if err != nil {
		// The row is already inserted, so the caller's rollback is what undoes
		// it. Rejecting here rather than after the commit is what keeps an
		// unrepresentable entry out of the trail.
		return domain.AuditEntry{}, fmt.Errorf("%w: the audit entry is not valid: %w", ErrDataAccess, err)
	}
	return entry, nil
}
