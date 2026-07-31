#!/usr/bin/env bash
# Offline adversarial drill for the shared host/Kubernetes snapshot format.
#
# The drill uses throwaway directories only. It proves exact-inventory restore,
# hash rejection, future-schema rejection, injected publication rollback, and
# cleanup of incomplete backup attempts without starting a model or cluster.

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd sqlite3 base

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

cp "${repo_dir}/agents/data/incidents.db" "${state_dir}/incidents.db"
sql "${state_dir}/runtime.db" \
	"CREATE TABLE drill_sessions (id TEXT PRIMARY KEY);
	 INSERT INTO drill_sessions VALUES ('drill-session-1');"
sql "${state_dir}/incidents.db" <<'SQL'
BEGIN;
UPDATE services SET status = 'operational' WHERE name = 'checkout';
INSERT INTO audit_log
    (schema_version, ts, actor, approved_by, rationale, context_summary,
     session_id, invocation_id, action, target, detail)
VALUES
    (1, '2026-01-01T00:00:00Z', 'agentops-agent', 'drill-operator', 'restore drill',
     'backup-drill.sh', 'drill-session-1', 'drill-invocation-1',
     'restart_service', 'checkout',
     'service restarted and marked operational (mock)');
COMMIT;
SQL

audit_before="$(sql "${state_dir}/incidents.db" 'SELECT count(*) FROM audit_log')"
sessions_before="$(sql "${state_dir}/runtime.db" 'SELECT count(*) FROM drill_sessions')"

# A corrupt later input must leave no published or hidden snapshot.
printf 'not a SQLite database\n' >"${state_dir}/zz-corrupt.db"
if STATE_BACKUP_TIMESTAMP=20990101T000000Z \
	"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}" \
	>"${drill_dir}/expected-backup-failure.log" 2>&1; then
	fail "drill failed: corrupt state unexpectedly produced a snapshot"
fi
rm "${state_dir}/zz-corrupt.db"
if [[ -e "${backup_root}/20990101T000000Z" ]] ||
	compgen -G "${backup_root}/.incomplete-20990101T000000Z.*" >/dev/null; then
	fail "drill failed: failed backup left a published or staged snapshot"
fi

STATE_BACKUP_TIMESTAMP=20990101T000001Z \
	"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}"
snapshot="${backup_root}/20990101T000001Z"
[[ -f "${snapshot}/manifest.json" && -f "${snapshot}/.complete" ]] ||
	fail "drill failed: snapshot metadata was not published"

# A hash mismatch must be rejected before any current file changes.
hash_target="${drill_dir}/hash-target"
mkdir "${hash_target}"
cp "${state_dir}"/*.db "${hash_target}/"
hash_before="$(sha256sum "${hash_target}"/*.db)"
printf 'tamper\n' >>"${snapshot}/runtime.db"
if "${scripts_dir}/restore-state.sh" "${snapshot}" "${hash_target}" \
	>"${drill_dir}/expected-hash-rejection.log" 2>&1; then
	fail "drill failed: restore accepted a bad database hash"
fi
hash_after="$(sha256sum "${hash_target}"/*.db)"
[[ "${hash_after}" == "${hash_before}" ]] ||
	fail "drill failed: hash rejection changed live state"

# Publish a fresh valid snapshot for restore fault injection and success.
STATE_BACKUP_TIMESTAMP=20990101T000002Z \
	"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}"
snapshot="${backup_root}/20990101T000002Z"

rollback_target="${drill_dir}/rollback-target"
mkdir "${rollback_target}"
cp "${state_dir}"/*.db "${rollback_target}/"
sql "${rollback_target}/obsolete.db" "CREATE TABLE stale (value TEXT); INSERT INTO stale VALUES ('keep-on-failure');"
rollback_before="$(sha256sum "${rollback_target}"/*.db)"
if STATE_RESTORE_FAIL_AFTER=1 \
	"${scripts_dir}/restore-state.sh" "${snapshot}" "${rollback_target}" \
	>"${drill_dir}/expected-publication-failure.log" 2>&1; then
	fail "drill failed: injected second-file publication failure was ignored"
fi
rollback_after="$(sha256sum "${rollback_target}"/*.db)"
[[ "${rollback_after}" == "${rollback_before}" ]] ||
	fail "drill failed: publication failure did not restore the complete prior generation"

# Successful restore publishes exactly the manifest inventory: obsolete.db
# must not silently survive an older snapshot.
"${scripts_dir}/restore-state.sh" "${snapshot}" "${rollback_target}"
[[ ! -e "${rollback_target}/obsolete.db" ]] ||
	fail "drill failed: database absent from the snapshot survived restore"
restored_names="$(find "${rollback_target}" -maxdepth 1 -name '*.db' -printf '%f\n' | sort)"
[[ "${restored_names}" == $'incidents.db\nruntime.db' ]] ||
	fail "drill failed: post-restore inventory differs from the manifest"

# A newer audit schema is rejected at backup without mutating its input.
sql "${state_dir}/incidents.db" <<'SQL'
INSERT INTO audit_log
    (schema_version, ts, actor, approved_by, rationale, context_summary,
     session_id, invocation_id, action, target, detail)
VALUES
    (99, '2099-01-01T00:00:00Z', 'future-agent', 'future-operator',
     'future schema drill', 'backup-drill.sh', 'future-session',
     'future-invocation', 'restart_service', 'checkout', 'future detail');
SQL
future_before="$(sha256sum "${state_dir}/incidents.db")"
if STATE_BACKUP_TIMESTAMP=20990101T000003Z \
	"${scripts_dir}/backup-state.sh" "${state_dir}" "${backup_root}" \
	>"${drill_dir}/expected-future-rejection.log" 2>&1; then
	fail "drill failed: backup accepted a future audit schema"
fi
future_after="$(sha256sum "${state_dir}/incidents.db")"
[[ "${future_after}" == "${future_before}" ]] ||
	fail "drill failed: future-schema rejection mutated its input"

audit_after="$(sql "${rollback_target}/incidents.db" 'SELECT count(*) FROM audit_log')"
sessions_after="$(sql "${rollback_target}/runtime.db" 'SELECT count(*) FROM drill_sessions')"
[[ "${audit_after}" == "${audit_before}" ]] ||
	fail "drill failed: audit rows ${audit_after} != ${audit_before}"
[[ "${sessions_after}" == "${sessions_before}" ]] ||
	fail "drill failed: session rows ${sessions_after} != ${sessions_before}"

echo "drill passed: versioned manifest, exact inventory, rollback, hashes, and schema compatibility"
