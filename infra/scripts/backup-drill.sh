#!/usr/bin/env bash
# Repeatable offline restore drill: prove a backup works before you need it.
#
# The drill runs entirely against a throwaway temporary directory — it never
# reads or writes agents/python/.state — and needs no cluster or model:
# 1. Seed throwaway state like the agent's first boot (dataset copy + a
#    runtime.db standing in for ADK sessions).
# 1. Approve a mock action: the same UPDATE + audit_log append that
#    agent.data.restart_service_with_audit commits in one transaction.
# 1. Force a mid-backup failure and prove no incomplete snapshot is published
#    or accepted by restore-state.sh.
# 1. Snapshot with backup-state.sh, then destroy the state directory (simulating
#    the volume dying).
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
    ('2026-01-01T00:00:00Z', 'agentops-agent', 'drill-operator', 'restore drill',
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

# 3. Failure path: a corrupt database fails after earlier files may have been
# copied, but the hidden staging directory must be cleaned and never published.
printf 'not a SQLite database\n' >"${state_dir}/zz-corrupt.db"
if "${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}" >"${drill_dir}/expected-failure.log" 2>&1; then
	echo "drill failed: corrupt database unexpectedly produced a snapshot" >&2
	exit 1
fi
rm "${state_dir}/zz-corrupt.db"
leftover_snapshot="$(find "${backup_root}" -mindepth 1 -maxdepth 1 -type d -print -quit)"
if [[ -n "${leftover_snapshot}" ]]; then
	echo "drill failed: failed backup left a published or staged snapshot" >&2
	exit 1
fi

# A same-second host backup must fail at the exclusive lock instead of nesting
# one staging directory inside another process's published snapshot.
locked_stamp="20990101T000000Z"
mkdir "${backup_root}/.lock-${locked_stamp}"
if STATE_BACKUP_TIMESTAMP="${locked_stamp}" \
	"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}" >"${drill_dir}/expected-lock-rejection.log" 2>&1; then
	echo "drill failed: backup ignored an existing timestamp lock" >&2
	exit 1
fi
if [[ -e "${backup_root}/${locked_stamp}" ]]; then
	echo "drill failed: lock-rejected backup published a snapshot" >&2
	exit 1
fi
rmdir "${backup_root}/.lock-${locked_stamp}"

# Restore verifies every source before replacing any destination. A corrupt
# later database must leave the entire existing target unchanged.
corrupt_snapshot="${backup_root}/20990101T000001Z"
restore_target="${drill_dir}/atomic-restore-target"
mkdir -p "${corrupt_snapshot}" "${restore_target}"
cp "${state_dir}/incidents.db" "${corrupt_snapshot}/incidents.db"
printf 'not a SQLite database\n' >"${corrupt_snapshot}/runtime.db"
printf 'completed_at=20990101T000001Z\ndatabases=2\n' >"${corrupt_snapshot}/.complete"
cp "${state_dir}/incidents.db" "${restore_target}/incidents.db"
cp "${state_dir}/runtime.db" "${restore_target}/runtime.db"
restore_before="$(cksum "${restore_target}/incidents.db" "${restore_target}/runtime.db")"
if "${scripts_dir}/restore-state.sh" "${corrupt_snapshot}" "${restore_target}" \
	>"${drill_dir}/expected-atomic-restore-rejection.log" 2>&1; then
	echo "drill failed: restore accepted a corrupt later database" >&2
	exit 1
fi
restore_after="$(cksum "${restore_target}/incidents.db" "${restore_target}/runtime.db")"
if [[ "${restore_after}" != "${restore_before}" ]]; then
	echo "drill failed: corrupt snapshot partially replaced existing state" >&2
	exit 1
fi
rm -rf "${corrupt_snapshot}" "${restore_target}"

# A completed marker also commits to the database count. A missing later file
# must be rejected before the existing target is changed or mixed with an older
# logical snapshot.
missing_snapshot="${backup_root}/20990101T000002Z"
missing_target="${drill_dir}/missing-restore-target"
mkdir -p "${missing_snapshot}" "${missing_target}"
cp "${state_dir}/incidents.db" "${missing_snapshot}/incidents.db"
printf 'completed_at=20990101T000002Z\ndatabases=2\n' >"${missing_snapshot}/.complete"
cp "${state_dir}/incidents.db" "${missing_target}/incidents.db"
cp "${state_dir}/runtime.db" "${missing_target}/runtime.db"
missing_before="$(cksum "${missing_target}/incidents.db" "${missing_target}/runtime.db")"
if "${scripts_dir}/restore-state.sh" "${missing_snapshot}" "${missing_target}" \
	>"${drill_dir}/expected-missing-restore-rejection.log" 2>&1; then
	echo "drill failed: restore accepted a snapshot with a missing database" >&2
	exit 1
fi
missing_after="$(cksum "${missing_target}/incidents.db" "${missing_target}/runtime.db")"
if [[ "${missing_after}" != "${missing_before}" ]]; then
	echo "drill failed: missing-database snapshot partially replaced existing state" >&2
	exit 1
fi
rm -rf "${missing_snapshot}" "${missing_target}"

# Retention and restore must ignore an unmarked directory even when it contains
# a database-shaped file.
incomplete_snapshot="${backup_root}/incomplete-drill"
mkdir -p "${incomplete_snapshot}"
cp "${state_dir}/incidents.db" "${incomplete_snapshot}/incidents.db"
if "${scripts_dir}/restore-state.sh" "${incomplete_snapshot}" "${drill_dir}/rejected-restore" \
	>"${drill_dir}/expected-restore-rejection.log" 2>&1; then
	echo "drill failed: restore accepted an incomplete snapshot" >&2
	exit 1
fi
# A SIGKILL can land after the marker write but before the atomic rename. The
# hidden name remains the publication guard even if that staging directory has
# a marker.
hidden_snapshot="${backup_root}/.incomplete-drill"
mkdir -p "${hidden_snapshot}"
cp "${state_dir}/incidents.db" "${hidden_snapshot}/incidents.db"
touch "${hidden_snapshot}/.complete"
if "${scripts_dir}/restore-state.sh" "${hidden_snapshot}" "${drill_dir}/rejected-hidden-restore" \
	>"${drill_dir}/expected-hidden-restore-rejection.log" 2>&1; then
	echo "drill failed: restore accepted a hidden unpublished snapshot" >&2
	exit 1
fi

# 4. Back up successfully, then destroy the state directory (the "volume died"
# moment). STATE_BACKUP_KEEP=1 exercises retention without pruning the hidden
# or unmarked incomplete directories above.
STATE_BACKUP_KEEP=1 "${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}"
if [[ ! -d "${incomplete_snapshot}" || ! -d "${hidden_snapshot}" ]]; then
	echo "drill failed: retention touched an incomplete snapshot" >&2
	exit 1
fi
snapshot_dir=""
for candidate in "${backup_root}"/*; do
	[[ -d "${candidate}" && -f "${candidate}/.complete" ]] || continue
	snapshot_dir="${candidate}"
done
if [[ -z "${snapshot_dir}" ]]; then
	echo "drill failed: no completed snapshot was published" >&2
	exit 1
fi
rm -rf "${state_dir}"

# 5. Restore and verify the evidence, not just the files.
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
