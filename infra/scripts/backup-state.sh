#!/usr/bin/env bash
# Snapshot the agent SQLite state (sessions, A2A tasks, audit trail) while it
# may be in use. Plain `cp` of a live database can capture a torn transaction;
# `VACUUM INTO` produces a transactionally consistent copy through SQLite.
#
# Usage: backup-state.sh [state_dir] [backup_root]
#   state_dir    defaults to agents/python/.state
#   backup_root  defaults to .state-backups (gitignored); each run writes a
#                UTC-timestamped snapshot directory and keeps the most recent
#                STATE_BACKUP_KEEP (default 7) snapshots.
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
state_dir="${1:-${repo_dir}/agents/python/.state}"
backup_root="${2:-${repo_dir}/.state-backups}"
keep="${STATE_BACKUP_KEEP:-7}"

if [[ ! -d "${state_dir}" ]]; then
	echo "error: state directory not found: ${state_dir}" >&2
	echo "hint: run the agent or MCP server once to initialize it" >&2
	exit 1
fi

snapshot_dir="${backup_root}/$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "${backup_root}"
# Exclusive create: a same-second rerun fails loudly instead of mixing files.
mkdir "${snapshot_dir}"

databases="$(find "${state_dir}" -maxdepth 1 -name '*.db' | sort)"

backed_up=0
while IFS= read -r database; do
	[[ -n "${database}" ]] || continue
	target="${snapshot_dir}/$(basename "${database}")"
	# The target path is interpolated into SQL, so refuse the one character
	# that could break out of the string literal.
	if [[ "${target}" == *"'"* ]]; then
		echo "error: backup path must not contain a single quote: ${target}" >&2
		exit 1
	fi
	sqlite3 -batch -init /dev/null "${database}" "VACUUM INTO '${target}'"
	integrity="$(sqlite3 -batch -init /dev/null "${target}" 'PRAGMA integrity_check')"
	if [[ "${integrity}" != "ok" ]]; then
		echo "error: integrity check failed for ${target}: ${integrity}" >&2
		exit 1
	fi
	echo "backed up $(basename "${database}") -> ${target}"
	backed_up=$((backed_up + 1))
done <<<"${databases}"

if [[ "${backed_up}" -eq 0 ]]; then
	rmdir "${snapshot_dir}"
	echo "error: no SQLite databases in ${state_dir}" >&2
	exit 1
fi

# Retention: keep only the newest snapshots so the lab cannot fill the disk.
find "${backup_root}" -mindepth 1 -maxdepth 1 -type d | sort -r |
	tail -n "+$((keep + 1))" | while IFS= read -r old_snapshot; do
	rm -rf "${old_snapshot}"
	echo "pruned ${old_snapshot}"
done

echo "snapshot complete: ${snapshot_dir}"
