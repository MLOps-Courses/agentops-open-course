#!/usr/bin/env bash

# Shared preamble for the repository's shell scripts.
#
# Source it as the first thing a script does:
#
#     source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
#
# It sets strict mode and provides the small prerequisite helpers shared by the
# repository scripts.

set -Eeuo pipefail

# log <message...> — progress on stderr, so stdout stays usable for a script's real output.
log() {
	printf '%s\n' "$*" >&2
}

# fail <message...> — report and exit non-zero.
fail() {
	printf '%s\n' "$*" >&2
	exit 1
}

# assert_eq <label> <got> <want> — compare one exact invariant and name it on failure.
assert_eq() {
	local label="$1"
	local got="$2"
	local want="$3"

	[[ "${got}" == "${want}" ]] || fail "${label}: got '${got}', want '${want}'"
}

# absolute_path <path> — resolve a possibly relative path against the caller's directory.
#
# Scripts that `cd` into a project before invoking a tool must resolve caller-supplied paths
# first, or a relative argument silently resolves against the project instead. The parent
# directory must exist; the leaf need not, so this also works for a path about to be created.
absolute_path() {
	local path="$1"
	local parent
	parent="$(cd -- "$(dirname -- "${path}")" 2>/dev/null && pwd)" ||
		fail "cannot resolve ${path}: its parent directory does not exist"
	printf '%s/%s\n' "${parent}" "$(basename -- "${path}")"
}

# json_flag <filter> <json> — read one JSON boolean as the literal text `true` or `false`.
#
# `jq -e` reports a false result as failure, so reading a boolean with it aborts the caller
# exactly when the flag is false — for source identity, that means every clean checkout.
# Validating the value's spelling keeps the fail-fast guard for a missing or non-boolean
# field while letting `false` through as data.
json_flag() {
	local filter="$1"
	local json="$2"

	# Select on the type, not the rendered text: `tostring` alone would also accept the
	# string "false", which is how a mis-typed producer would slip a flag past this guard.
	jq -er "${filter} | select(type == \"boolean\") | tostring" <<<"${json}" ||
		fail "expected a JSON boolean at ${filter}"
}

# require_cmd <command> [doctor-profile] — assert a tool is on PATH, naming how to install it.
require_cmd() {
	local command_name="$1"
	local profile="${2:-}"

	if command -v "${command_name}" >/dev/null 2>&1; then
		return 0
	fi

	# The remedy has to name a tier the course actually teaches. Bare `mise install`
	# is taught nowhere, and mise.toml's own comment on install:maintainer warns that
	# it also materializes a contributor's global tool catalog.
	local tier
	case "${profile}" in
	base) tier="mise run install" ;;
	gateway | platform | gcp) tier="mise run install:platform" ;;
	validation) tier="mise run install:validation" ;;
	*) tier="mise run install" ;;
	esac

	if [[ -n ${profile} ]]; then
		fail "missing ${command_name}: run '${tier}', then 'mise run doctor:${profile}' to check the whole tier"
	fi
	fail "missing ${command_name}: run 'mise run install' to materialize the pinned toolchain"
}

# require_host_cmd <command> <remedy> — assert a prerequisite intentionally
# owned by the host rather than pretending a repository installer can provide it.
require_host_cmd() {
	local command_name="$1"
	local remedy="$2"

	command -v "${command_name}" >/dev/null 2>&1 || fail "missing ${command_name}: ${remedy}"
}

# sha256_file <path> — print one portable SHA-256 digest.
sha256_file() {
	local path="$1"

	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "${path}" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "${path}" | awk '{ print $1 }'
	else
		fail "missing SHA-256 tool: install sha256sum or shasum from a reviewed host package source"
	fi
}

# verify_sha256 <path> <expected> <label> — fail closed on unreviewed bytes.
verify_sha256() {
	local path="$1"
	local wanted_digest="$2"
	local label="$3"
	local actual

	actual="$(sha256_file "${path}")"
	[[ ${actual} == "${wanted_digest}" ]] ||
		fail "${label} checksum mismatch (expected ${wanted_digest}, got ${actual})"
}

# git_in <dir> <args...> — run git against exactly <dir>'s repository.
#
# `git -C` sets the working directory but loses to an inherited GIT_DIR, and git
# exports GIT_DIR to every hook it runs. A caller reached from a hook would
# otherwise inspect — or write to — the contributor's repository while believing
# it addressed the directory it named.
git_in() {
	local directory="$1"
	shift
	env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE -u GIT_OBJECT_DIRECTORY \
		-u GIT_ALTERNATE_OBJECT_DIRECTORIES -u GIT_COMMON_DIR -u GIT_PREFIX \
		git -C "${directory}" "$@"
}

# verify_git_binary_install <dir> <commit> <binary> <sha256> <label> — bind
# repeat installs to a clean reviewed checkout and the exact bytes Helm executes.
verify_git_binary_install() {
	local directory="$1"
	local wanted_commit="$2"
	local binary="$3"
	local wanted_sha256="$4"
	local label="$5"
	local actual_commit
	local worktree_status

	[[ -d ${directory} && ! -L ${directory} ]] || fail "${label} directory is missing or is a link"
	# git_in, not `git -C`: an inherited GIT_DIR would make both checks below
	# report on the caller's repository, and a clean course checkout would then
	# certify a plugin checkout nobody looked at.
	actual_commit="$(git_in "${directory}" rev-parse HEAD 2>/dev/null)" ||
		fail "${label} source commit is unavailable"
	[[ ${actual_commit} == "${wanted_commit}" ]] ||
		fail "${label} source commit mismatch (expected ${wanted_commit}, got ${actual_commit})"
	worktree_status="$(git_in "${directory}" status --porcelain=v1 --untracked-files=all 2>/dev/null)" ||
		fail "${label} source checkout status is unavailable"
	[[ -z ${worktree_status} ]] || fail "${label} source checkout is dirty"
	[[ -f ${directory}/${binary} && ! -L ${directory}/${binary} ]] ||
		fail "${label} executable is missing or is a link"
	verify_sha256 "${directory}/${binary}" "${wanted_sha256}" "${label} executable"
}

# require_cgroup_v2 <cgroup-root> — Kubernetes 1.35 removed cgroup v1 support.
# Check the host before the pinned k3s line creates a partial cluster.
require_cgroup_v2() {
	local cgroup_root="$1"

	if [[ -r ${cgroup_root}/cgroup.controllers ]]; then
		return 0
	fi
	fail "cgroup v2 required for pinned Kubernetes; enable the unified cgroup hierarchy before running local k3d"
}
