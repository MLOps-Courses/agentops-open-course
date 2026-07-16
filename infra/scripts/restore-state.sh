#!/usr/bin/env bash
# Restore the agent SQLite state from a snapshot taken by backup-state.sh.
#
# Preconditions (this script stops nothing itself):
# 1. Stop every writer first: the A2A server (`mise run a2a`), the MCP server
#    (`mise run mcp` / `mcp:http`), and any `adk run` / `adk web` session.
# 1. Pick one completed snapshot directory (it contains `.complete`), e.g. the
#    newest visible entry under .state-backups/.
#
# Each database is verified (`PRAGMA integrity_check`), staged next to its
# destination, and moved into place atomically; stale journal/WAL sidecar
# files are removed so SQLite cannot replay them against the restored copy.
#
# Usage: restore-state.sh <snapshot_dir> [state_dir]
#   state_dir  defaults to agents/python/.state
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
snapshot_dir="${1:?usage: restore-state.sh <snapshot_dir> [state_dir]}"
state_dir="${2:-${repo_dir}/agents/python/.state}"
restore_staging=""

cleanup_restore_staging() {
	if [[ -n "${restore_staging}" && -d "${restore_staging}" ]]; then
		rm -rf "${restore_staging}"
	fi
}

trap cleanup_restore_staging EXIT

if [[ ! -d "${snapshot_dir}" ]]; then
	echo "error: snapshot directory not found: ${snapshot_dir}" >&2
	exit 1
fi
if [[ "$(basename "${snapshot_dir}")" == .* ]]; then
	echo "error: snapshot is still hidden/unpublished: ${snapshot_dir}" >&2
	exit 1
fi
if [[ ! -f "${snapshot_dir}/.complete" ]]; then
	echo "error: snapshot is incomplete or unpublished (missing .complete): ${snapshot_dir}" >&2
	exit 1
fi
expected_databases="$(sed -n 's/^databases=//p' "${snapshot_dir}/.complete")"
if [[ ! "${expected_databases}" =~ ^[1-9][0-9]*$ ]]; then
	echo "error: snapshot marker has no single positive databases count: ${snapshot_dir}/.complete" >&2
	exit 1
fi

mkdir -p "${state_dir}"

databases="$(find "${snapshot_dir}" -maxdepth 1 -name '*.db' | sort)"

# Verify and stage every database before replacing any live file. A corrupt
# later database must not leave a partially restored logical snapshot.
staged=0
restore_staging="$(mktemp -d "${state_dir}/.restore.XXXXXX")"
while IFS= read -r database; do
	[[ -n "${database}" ]] || continue
	name="$(basename "${database}")"
	integrity="$(sqlite3 -batch -init /dev/null "${database}" 'PRAGMA integrity_check')"
	if [[ "${integrity}" != "ok" ]]; then
		echo "error: snapshot ${name} failed integrity check: ${integrity}" >&2
		exit 1
	fi
	cp "${database}" "${restore_staging}/${name}"
	staged=$((staged + 1))
done <<<"${databases}"

if [[ "${staged}" -eq 0 ]]; then
	echo "error: no SQLite databases in snapshot ${snapshot_dir}" >&2
	exit 1
fi
if [[ "${staged}" != "${expected_databases}" ]]; then
	echo "error: snapshot ${snapshot_dir} declares ${expected_databases} database(s), found ${staged}" >&2
	exit 1
fi

restored=0
staged_databases="$(find "${restore_staging}" -maxdepth 1 -name '*.db' | sort)"
while IFS= read -r database; do
	[[ -n "${database}" ]] || continue
	name="$(basename "${database}")"
	rm -f "${state_dir}/${name}-wal" "${state_dir}/${name}-shm" "${state_dir}/${name}-journal"
	mv "${database}" "${state_dir}/${name}"
	echo "restored ${name} -> ${state_dir}/${name}"
	restored=$((restored + 1))
done <<<"${staged_databases}"

rmdir "${restore_staging}"
restore_staging=""

# Prove the audit evidence made it back, not merely that files exist.
if [[ -f "${state_dir}/incidents.db" ]]; then
	audit_rows="$(sqlite3 -batch -init /dev/null "${state_dir}/incidents.db" 'SELECT count(*) FROM audit_log')"
	echo "audit_log rows after restore: ${audit_rows}"
fi

echo "restore complete: ${state_dir}"
