#!/usr/bin/env bash
# Restore the agent SQLite state from a snapshot taken by backup-state.sh.
#
# Preconditions (this script stops nothing itself):
# 1. Stop every writer first: the A2A server (`mise run a2a`), the MCP server
#    (`mise run mcp` / `mcp:http`), and any `adk run` / `adk web` session.
# 1. Pick one snapshot directory, e.g. the newest under .state-backups/.
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

if [[ ! -d "${snapshot_dir}" ]]; then
	echo "error: snapshot directory not found: ${snapshot_dir}" >&2
	exit 1
fi

mkdir -p "${state_dir}"

databases="$(find "${snapshot_dir}" -maxdepth 1 -name '*.db' | sort)"

restored=0
while IFS= read -r database; do
	[[ -n "${database}" ]] || continue
	name="$(basename "${database}")"
	integrity="$(sqlite3 -batch -init /dev/null "${database}" 'PRAGMA integrity_check')"
	if [[ "${integrity}" != "ok" ]]; then
		echo "error: snapshot ${name} failed integrity check: ${integrity}" >&2
		exit 1
	fi
	# Stage in the destination directory so the final rename is atomic.
	staged="$(mktemp "${state_dir}/.restore-${name}.XXXXXX")"
	cp "${database}" "${staged}"
	rm -f "${state_dir}/${name}-wal" "${state_dir}/${name}-shm" "${state_dir}/${name}-journal"
	mv "${staged}" "${state_dir}/${name}"
	echo "restored ${name} -> ${state_dir}/${name}"
	restored=$((restored + 1))
done <<<"${databases}"

if [[ "${restored}" -eq 0 ]]; then
	echo "error: no SQLite databases in snapshot ${snapshot_dir}" >&2
	exit 1
fi

# Prove the audit evidence made it back, not merely that files exist.
if [[ -f "${state_dir}/incidents.db" ]]; then
	audit_rows="$(sqlite3 -batch -init /dev/null "${state_dir}/incidents.db" 'SELECT count(*) FROM audit_log')"
	echo "audit_log rows after restore: ${audit_rows}"
fi

echo "restore complete: ${state_dir}"
