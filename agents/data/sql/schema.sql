-- AgentOps Open Course — Ops Copilot dataset schema (SQLite).
-- Single source of truth for the AgentOps Open Course reference agent (agents/python).
-- Rebuild incidents.db from this file + seed.sql with `mise run build` (see ../mise.toml).

PRAGMA foreign_keys = ON;

DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS incidents;
DROP TABLE IF EXISTS services;

-- The fictional services the Ops Copilot watches over.
CREATE TABLE services (
    name        TEXT PRIMARY KEY,                          -- stable slug, e.g. "checkout"
    description TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('operational', 'degraded', 'down')),
    owner       TEXT NOT NULL                              -- owning team
);

-- Incidents raised against those services. `runbook` links to runbooks/<runbook>.md.
CREATE TABLE incidents (
    id          TEXT PRIMARY KEY,                          -- human id, e.g. "INC-001"
    service     TEXT NOT NULL REFERENCES services (name),
    title       TEXT NOT NULL,
    severity    TEXT NOT NULL CHECK (severity IN ('SEV1', 'SEV2', 'SEV3')),
    status      TEXT NOT NULL CHECK (status IN ('open', 'investigating', 'resolved')),
    runbook     TEXT NOT NULL,                             -- runbook slug (knowledge base key)
    opened_at   TEXT NOT NULL,                             -- ISO-8601 UTC
    resolved_at TEXT,                                      -- ISO-8601 UTC, NULL while unresolved
    summary     TEXT NOT NULL
);

-- Append-only trail of the mock actions the agent performs (restart, resolve, ...).
-- Guarded actions (Ch. 4.5) write here instead of touching real infrastructure.
CREATE TABLE audit_log (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              TEXT NOT NULL,                         -- ISO-8601 UTC of the action
    actor           TEXT NOT NULL,                         -- executing agent, e.g. "ops-copilot"
    approved_by     TEXT NOT NULL,                         -- authenticated ADK user that confirmed it
    rationale       TEXT NOT NULL,                         -- the approver's stated reason (HITL, Ch. 4.5)
    context_summary TEXT NOT NULL,                         -- the decision context recorded at approval time
    session_id      TEXT NOT NULL,
    invocation_id   TEXT NOT NULL,
    action          TEXT NOT NULL,                         -- e.g. "restart_service", "resolve_incident"
    target          TEXT NOT NULL,                         -- service name or incident id
    detail          TEXT NOT NULL
);

-- SQLite is not a tamper-proof audit system, but these triggers make the course's
-- application log append-only: existing rows cannot be rewritten or deleted.
CREATE TRIGGER audit_log_no_update
BEFORE UPDATE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE TRIGGER audit_log_no_delete
BEFORE DELETE ON audit_log
BEGIN
    SELECT RAISE(ABORT, 'audit_log is append-only');
END;

CREATE INDEX idx_incidents_status ON incidents (status);
CREATE INDEX idx_incidents_service ON incidents (service);
