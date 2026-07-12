#!/usr/bin/env bash
# Repeatable offline restore drill: prove a backup works before you need it.
#
# The drill runs entirely against a throwaway temporary directory — it never
# reads or writes agents/python/.state — and needs no cluster or model:
# 1. Seed throwaway state like the agent's first boot (dataset copy + a
#    runtime.db standing in for ADK sessions).
# 1. Approve a mock action: the same UPDATE + audit_log append that
#    agent.data.restart_service_with_audit commits in one transaction.
# 1. Snapshot with backup-state.sh, then destroy the state directory
#    (simulating the volume dying).
# 1. Restore with restore-state.sh and assert row counts and the last audit
#    entry survived.
#
# Usage: backup-drill.sh
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
scripts_dir="${repo_dir}/infra/scripts"
drill_dir="$(mktemp -d)"
trap 'rm -rf "${drill_dir}"' EXIT

state_dir="${drill_dir}/state"
backup_root="${drill_dir}/backups"
mkdir -p "${state_dir}"

sql() {
	sqlite3 -batch -init /dev/null "$@"
}

# 1. Seed throwaway state (the committed dataset is copied, never mutated).
cp "${repo_dir}/agents/data/incidents.db" "${state_dir}/incidents.db"
sql "${state_dir}/runtime.db" \
	"CREATE TABLE drill_sessions (id TEXT PRIMARY KEY);
	 INSERT INTO drill_sessions VALUES ('drill-session-1');"

# 2. Mutate: approve a mock restart_service action with its audit evidence.
sql "${state_dir}/incidents.db" <<'SQL'
BEGIN;
UPDATE services SET status = 'operational' WHERE name = 'checkout';
INSERT INTO audit_log
    (ts, actor, approved_by, rationale, context_summary,
     session_id, invocation_id, action, target, detail)
VALUES
    ('2026-01-01T00:00:00Z', 'ops-copilot', 'drill-operator', 'restore drill',
     'backup-drill.sh', 'drill-session-1', 'drill-invocation-1',
     'restart_service', 'checkout',
     'service restarted and marked operational (mock)');
COMMIT;
SQL

audit_before="$(sql "${state_dir}/incidents.db" 'SELECT count(*) FROM audit_log')"
sessions_before="$(sql "${state_dir}/runtime.db" 'SELECT count(*) FROM drill_sessions')"
if [[ "${audit_before}" -lt 1 ]]; then
	echo "drill setup failed: expected at least one audit row, got ${audit_before}" >&2
	exit 1
fi

# 3. Back up, then destroy the state directory (the "volume died" moment).
"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}"
snapshot_dir="$(find "${backup_root}" -mindepth 1 -maxdepth 1 -type d | sort | tail -n 1)"
rm -rf "${state_dir}"

# 4. Restore and verify the evidence, not just the files.
"${scripts_dir}/restore-state.sh" "${snapshot_dir}" "${state_dir}"

audit_after="$(sql "${state_dir}/incidents.db" 'SELECT count(*) FROM audit_log')"
sessions_after="$(sql "${state_dir}/runtime.db" 'SELECT count(*) FROM drill_sessions')"
last_entry="$(sql "${state_dir}/incidents.db" \
	"SELECT action || ' ' || target || ' by ' || approved_by
	 FROM audit_log ORDER BY id DESC LIMIT 1")"

if [[ "${audit_after}" != "${audit_before}" ]]; then
	echo "drill failed: audit_log rows ${audit_after} != ${audit_before}" >&2
	exit 1
fi
if [[ "${sessions_after}" != "${sessions_before}" ]]; then
	echo "drill failed: session rows ${sessions_after} != ${sessions_before}" >&2
	exit 1
fi
if [[ "${last_entry}" != "restart_service checkout by drill-operator" ]]; then
	echo "drill failed: unexpected last audit entry: ${last_entry}" >&2
	exit 1
fi

echo "drill passed: ${audit_after} audit row(s) and ${sessions_after} session row(s) survived backup + restore"
