#!/usr/bin/env bash
# Restore the exact SQLite generation recorded by one completed snapshot.
#
# Stop every A2A, MCP, ADK, or other state writer before running this command.
# The restore validates the full inventory before publication. A durable journal
# either rolls the byte-identical prior generation back after interruption or
# preserves a fully committed new generation on the next state command.
#
# Usage: restore-state.sh <snapshot_dir> [state_dir]

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd go base

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
snapshot_dir="$(absolute_path "${1:?usage: restore-state.sh <snapshot_dir> [state_dir]}")"
state_dir="$(absolute_path "${2:-${repo_dir}/agents/go/.state}")"

# `go run` compiles the module's own state CLI, so a checkout restores with exactly the
# source under review — the same subcommand the deployed image serves as `agent state restore`.
cd "${repo_dir}/agents/go"
exec go run ./cmd/agent state restore "${snapshot_dir}" \
	--state-dir "${state_dir}"
