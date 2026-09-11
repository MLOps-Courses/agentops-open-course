#!/usr/bin/env bash
# Publish a versioned, integrity-checked snapshot of every agent SQLite database.
#
# Usage: backup-state.sh [state_dir] [backup_root]
#   state_dir    defaults to agents/go/.state
#   backup_root  defaults to .state-backups

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd go base
require_cmd jq base

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
state_dir="$(absolute_path "${1:-${repo_dir}/agents/go/.state}")"
backup_root="$(absolute_path "${2:-${repo_dir}/.state-backups}")"

# `go run` has no release metadata of its own, so resolve the checkout once and
# inject the same validated tuple an image build receives. An ambient variable
# cannot relabel the snapshot independently of the binary that writes it.
source_json="$(go -C "${repo_dir}/tools" run ./cmd/source-identity --root "${repo_dir}" --mode development)"
source_identity="$(jq -er '.display' <<<"${source_json}")"
source_revision="$(jq -er '.revision // ""' <<<"${source_json}")"
source_tree_digest="$(jq -er '.tree_digest' <<<"${source_json}")"
source_dirty="$(json_flag '.dirty' "${source_json}")"
build_timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
readonly source_identity source_revision source_tree_digest source_dirty build_timestamp

buildinfo_package="github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
link_flags="-X ${buildinfo_package}.buildMode=development \
-X ${buildinfo_package}.version=development \
-X ${buildinfo_package}.sourceIdentity=${source_identity} \
-X ${buildinfo_package}.revision=${source_revision} \
-X ${buildinfo_package}.treeDigest=${source_tree_digest} \
-X ${buildinfo_package}.buildTimestamp=${build_timestamp} \
-X ${buildinfo_package}.dirty=${source_dirty}"
readonly buildinfo_package link_flags

# `go run` compiles the module's own state CLI, so a checkout snapshots with exactly the
# source under review — the same subcommand the deployed image serves as `agent state backup`.
cd "${repo_dir}/agents/go"
exec go run -ldflags "${link_flags}" ./cmd/agent state backup \
	--state-dir "${state_dir}" \
	--backup-root "${backup_root}"
