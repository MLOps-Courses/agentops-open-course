#!/usr/bin/env bash

set -euo pipefail

if (($# != 4)); then
	echo "usage: $0 VERSION_JSON LABELS_JSON AGENT_CARD_JSON MANIFEST_JSON" >&2
	exit 2
fi

readonly version_json="$1"
readonly labels_json="$2"
readonly agent_card_json="$3"
readonly manifest_json="$4"

for input in \
	"${version_json}" \
	"${labels_json}" \
	"${agent_card_json}" \
	"${manifest_json}"; do
	[[ -f ${input} ]] || {
		echo "platform build identity input is not a regular file: ${input}" >&2
		exit 1
	}
done

for name in \
	AGENT_BUILD_MODE \
	AGENT_SOURCE_COMMIT \
	AGENT_SOURCE_REVISION \
	AGENT_SOURCE_TREE_DIGEST \
	AGENT_SOURCE_DIRTY \
	OCI_CREATED \
	OCI_VERSION; do
	[[ -n ${!name:-} ]] || {
		echo "platform build identity requires ${name}" >&2
		exit 1
	}
done

[[ ${AGENT_BUILD_MODE} == release ]] || {
	echo "platform build identity requires release mode" >&2
	exit 1
}
[[ ${AGENT_SOURCE_DIRTY} == false ]] || {
	echo "platform build identity requires a clean candidate" >&2
	exit 1
}
[[ ${AGENT_SOURCE_COMMIT} == "${AGENT_SOURCE_REVISION}" ]] || {
	echo "platform source identity and revision disagree" >&2
	exit 1
}
[[ ${AGENT_SOURCE_REVISION} =~ ^[0-9a-f]{40}$ ]] || {
	echo "platform source revision is not a full lowercase commit" >&2
	exit 1
}
[[ ${AGENT_SOURCE_TREE_DIGEST} =~ ^sha256:[0-9a-f]{64}$ ]] || {
	echo "platform source tree digest is malformed" >&2
	exit 1
}
[[ ${OCI_VERSION} =~ ^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
	echo "platform version is not a stable semantic version" >&2
	exit 1
}
jq -en --arg timestamp "${OCI_CREATED}" \
	'$timestamp | fromdateiso8601' >/dev/null || {
	echo "platform build timestamp is not canonical UTC RFC3339" >&2
	exit 1
}

expected="$(
	jq -cn \
		--arg mode "${AGENT_BUILD_MODE}" \
		--arg version "${OCI_VERSION}" \
		--arg source_identity "${AGENT_SOURCE_COMMIT}" \
		--arg revision "${AGENT_SOURCE_REVISION}" \
		--arg tree_digest "${AGENT_SOURCE_TREE_DIGEST}" \
		--arg build_timestamp "${OCI_CREATED}" \
		'{
		  mode: $mode,
		  version: $version,
		  source_identity: $source_identity,
		  revision: $revision,
		  tree_digest: $tree_digest,
		  build_timestamp: $build_timestamp,
		  dirty: false
		}'
)"
readonly expected

if ! jq -e --argjson expected "${expected}" '
  {
    mode,
    version,
    source_identity,
    revision,
    tree_digest,
    build_timestamp,
    dirty
  } == $expected
' "${version_json}" >/dev/null; then
	echo "running agent build identity does not match the candidate" >&2
	exit 1
fi

if ! jq -e --argjson expected "${expected}" '
  .["dev.fmind.agentops.source-dirty"] == "false" and
  .["org.opencontainers.image.revision"] == $expected.revision and
  ({
    mode: .["dev.fmind.agentops.build-mode"],
    version: .["org.opencontainers.image.version"],
    source_identity: .["dev.fmind.agentops.source-identity"],
    revision: .["dev.fmind.agentops.source-revision"],
    tree_digest: .["dev.fmind.agentops.source-tree-digest"],
    build_timestamp: .["org.opencontainers.image.created"],
    dirty: (.["dev.fmind.agentops.source-dirty"] == "true")
  } == $expected)
' "${labels_json}" >/dev/null; then
	echo "deployed image labels do not match the candidate" >&2
	exit 1
fi

if ! jq -e --arg version "${OCI_VERSION}" '
  .name == "AgentOps Agent" and .version == $version
' "${agent_card_json}" >/dev/null; then
	echo "AgentCard identity does not match the candidate" >&2
	exit 1
fi

if ! jq -e --argjson expected "${expected}" '
  .source as $source |
  ($source | {
    mode,
    version,
    source_identity,
    revision,
    tree_digest,
    build_timestamp,
    dirty
  }) == $expected and
  $source.application == "agentops-agent" and
  $source.commit == $expected.source_identity
' "${manifest_json}" >/dev/null; then
	echo "backup manifest build identity does not match the candidate" >&2
	exit 1
fi

echo "deployed build identity agrees across CLI, OCI, AgentCard, and backup"
