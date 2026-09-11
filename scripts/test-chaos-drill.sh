#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
seed_db="${repo_root}/agents/data/incidents.db"
seed_runbook="${repo_root}/agents/data/runbooks/service-down.md"
tmp_dir="$(mktemp -d /tmp/agentops-chaos-test.XXXXXX)"
trap 'rm -r -- "${tmp_dir}"' EXIT

cp -- "${seed_db}" "${tmp_dir}/incidents.before"
cp -- "${seed_runbook}" "${tmp_dir}/runbook.before"

database_output="$("${repo_root}/scripts/chaos-drill.sh" database 2>&1)"
grep -Fq "the seed was untouched" <<<"${database_output}"
cmp --silent "${seed_db}" "${tmp_dir}/incidents.before"

runbook_output="$("${repo_root}/scripts/chaos-drill.sh" runbook 2>&1)"
grep -Fq "the committed runbook was untouched" <<<"${runbook_output}"
cmp --silent "${seed_runbook}" "${tmp_dir}/runbook.before"
