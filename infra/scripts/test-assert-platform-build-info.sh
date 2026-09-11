#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
fixture_parent="$(realpath -e -- "${TMPDIR:-/tmp}")"
fixture_dir="$(mktemp -d "${fixture_parent}/agentops-platform-build-info.XXXXXX")"
readonly script_dir fixture_parent fixture_dir

cleanup() {
	[[ -d ${fixture_dir} && ! -L ${fixture_dir} ]] || {
		echo "refusing to clean an invalid fixture directory: ${fixture_dir}" >&2
		return 1
	}
	[[ $(dirname -- "${fixture_dir}") == "${fixture_parent}" ]] || {
		echo "refusing to clean a fixture outside ${fixture_parent}: ${fixture_dir}" >&2
		return 1
	}
	[[ $(basename -- "${fixture_dir}") =~ ^agentops-platform-build-info\.[A-Za-z0-9]{6}$ ]] || {
		echo "refusing to clean an unexpected fixture name: ${fixture_dir}" >&2
		return 1
	}
	rm -r -- "${fixture_dir:?}"
}
trap cleanup EXIT

grep -Fq 'assert-platform-build-info.sh' "${script_dir}/platform-backup-drill.sh"
grep -Fq 'platform-backup-drill.sh' "${script_dir}/../../.github/workflows/platform.yml"
grep -Fq '"OTEL_TRACES_SAMPLER=always_off"' \
	"${script_dir}/../../.github/workflows/platform.yml"
grep -Fq 'http://127.0.0.1:3200/api/search' \
	"${script_dir}/../../.github/workflows/platform.yml"
grep -Fq 'q={ resource.service.name = "agentops-agent" }' \
	"${script_dir}/../../.github/workflows/platform.yml"
grep -Fq '.traces | length == 0' \
	"${script_dir}/../../.github/workflows/platform.yml"
grep -Fq 'for _ in {1..30}; do' \
	"${script_dir}/../../.github/workflows/platform.yml"
grep -Fq 'traffic_window_end + 1' \
	"${script_dir}/../../.github/workflows/platform.yml"
if grep -Fq 'TRACE_JSON' "${script_dir}/assert-platform-build-info.sh"; then
	echo "build identity proof must not require a trace when tracing is disabled" >&2
	exit 1
fi
if grep -Fq 'tempo-trace' "${script_dir}/platform-backup-drill.sh"; then
	echo "backup identity proof must not depend on an agent trace" >&2
	exit 1
fi

readonly revision=5dd9c33494a37928a0f4ebe66ec57d0081f7d541
readonly tree_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
readonly created=2026-08-08T15:16:25Z
readonly version=1.0.0

export AGENT_BUILD_MODE=release
export AGENT_SOURCE_COMMIT="${revision}"
export AGENT_SOURCE_REVISION="${revision}"
export AGENT_SOURCE_TREE_DIGEST="${tree_digest}"
export AGENT_SOURCE_DIRTY=false
export OCI_CREATED="${created}"
export OCI_VERSION="${version}"

write_fixtures() {
	cat >"${fixture_dir}/version.json" <<EOF
{"mode":"release","version":"${version}","source_identity":"${revision}","revision":"${revision}","tree_digest":"${tree_digest}","build_timestamp":"${created}","dirty":false}
EOF
	cat >"${fixture_dir}/labels.json" <<EOF
{"org.opencontainers.image.created":"${created}","org.opencontainers.image.version":"${version}","org.opencontainers.image.revision":"${revision}","dev.fmind.agentops.build-mode":"release","dev.fmind.agentops.source-identity":"${revision}","dev.fmind.agentops.source-revision":"${revision}","dev.fmind.agentops.source-tree-digest":"${tree_digest}","dev.fmind.agentops.source-dirty":"false"}
EOF
	cat >"${fixture_dir}/agent-card.json" <<EOF
{"name":"AgentOps Agent","version":"${version}"}
EOF
	cat >"${fixture_dir}/manifest.json" <<EOF
{"source":{"application":"agentops-agent","mode":"release","version":"${version}","source_identity":"${revision}","commit":"${revision}","revision":"${revision}","tree_digest":"${tree_digest}","build_timestamp":"${created}","dirty":false},"databases":[]}
EOF
}

assert_fixtures() {
	"${script_dir}/assert-platform-build-info.sh" \
		"${fixture_dir}/version.json" \
		"${fixture_dir}/labels.json" \
		"${fixture_dir}/agent-card.json" \
		"${fixture_dir}/manifest.json" >/dev/null
}

write_fixtures
assert_fixtures

jq '.source.tree_digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"' \
	"${fixture_dir}/manifest.json" >"${fixture_dir}/manifest-mismatch.json"
mv "${fixture_dir}/manifest-mismatch.json" "${fixture_dir}/manifest.json"
if assert_fixtures 2>"${fixture_dir}/mismatch-error.txt"; then
	echo "mismatched backup identity unexpectedly passed" >&2
	exit 1
fi
grep -Fq 'backup manifest build identity does not match the candidate' \
	"${fixture_dir}/mismatch-error.txt"

write_fixtures
jq '.version = "9.9.9"' "${fixture_dir}/agent-card.json" >"${fixture_dir}/card-mismatch.json"
mv "${fixture_dir}/card-mismatch.json" "${fixture_dir}/agent-card.json"
if assert_fixtures 2>"${fixture_dir}/card-error.txt"; then
	echo "mismatched AgentCard version unexpectedly passed" >&2
	exit 1
fi
grep -Fq 'AgentCard identity does not match the candidate' "${fixture_dir}/card-error.txt"

write_fixtures
jq 'del(.["dev.fmind.agentops.source-dirty"])' \
	"${fixture_dir}/labels.json" >"${fixture_dir}/labels-missing.json"
mv "${fixture_dir}/labels-missing.json" "${fixture_dir}/labels.json"
if assert_fixtures 2>"${fixture_dir}/labels-error.txt"; then
	echo "image labels without dirty state unexpectedly passed" >&2
	exit 1
fi
grep -Fq 'deployed image labels do not match the candidate' "${fixture_dir}/labels-error.txt"

write_fixtures
jq '.["dev.fmind.agentops.source-dirty"] = "False"' \
	"${fixture_dir}/labels.json" >"${fixture_dir}/labels-malformed.json"
mv "${fixture_dir}/labels-malformed.json" "${fixture_dir}/labels.json"
if assert_fixtures 2>"${fixture_dir}/labels-malformed-error.txt"; then
	echo "image labels with malformed dirty state unexpectedly passed" >&2
	exit 1
fi
grep -Fq 'deployed image labels do not match the candidate' \
	"${fixture_dir}/labels-malformed-error.txt"

write_fixtures
jq '.["org.opencontainers.image.revision"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"' \
	"${fixture_dir}/labels.json" >"${fixture_dir}/labels-revision.json"
mv "${fixture_dir}/labels-revision.json" "${fixture_dir}/labels.json"
if assert_fixtures 2>"${fixture_dir}/labels-revision-error.txt"; then
	echo "image labels with a mismatched OCI revision unexpectedly passed" >&2
	exit 1
fi
grep -Fq 'deployed image labels do not match the candidate' \
	"${fixture_dir}/labels-revision-error.txt"

echo "platform build identity assertions passed"
