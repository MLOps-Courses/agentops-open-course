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

# require_cmd <command> [doctor-profile] — assert a tool is on PATH, naming how to install it.
require_cmd() {
	local command_name="$1"
	local profile="${2:-}"

	if command -v "${command_name}" >/dev/null 2>&1; then
		return 0
	fi

	if [[ -n ${profile} ]]; then
		fail "missing ${command_name}: run 'mise install', then 'mise run doctor:${profile}' to check the whole tier"
	fi
	fail "missing ${command_name}: run 'mise install' to materialize the pinned toolchain"
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
