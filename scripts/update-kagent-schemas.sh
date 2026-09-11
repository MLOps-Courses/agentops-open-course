#!/usr/bin/env bash

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd helm platform
require_cmd yq platform
require_cmd jq base

mode=${1:---check}
case "${mode}" in
--check | --write) ;;
*) fail "usage: $0 [--check|--write]" ;;
esac

readonly schema_dir=infra/kagent/schemas
chart_ref="$(
	yq -r '.releases[] | select(.name == "kagent-crds") | .chart' infra/helmfile.yaml
)"
readonly chart_ref
if [[ ${chart_ref} != oci://*@sha256:* ]]; then
	fail "kagent CRD chart must use an immutable OCI digest: ${chart_ref}"
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/agentops-kagent-schemas.XXXXXX")
readonly work_dir
trap 'rm -rf -- "${work_dir}"' EXIT

helm template kagent-crds "${chart_ref}" --namespace kagent >"${work_dir}/crds.yaml"

for kind in Agent ModelConfig RemoteMCPServer; do
	output="${work_dir}/${kind,,}_v1alpha2.json"
	yq -o=json \
		'select(
			.kind == "CustomResourceDefinition" and
			.spec.names.kind == "'"${kind}"'"
		) |
		.spec.versions[] |
		select(.name == "v1alpha2" and .served == true) |
		.schema.openAPIV3Schema' \
		"${work_dir}/crds.yaml" |
		jq -e \
			--arg source "${chart_ref}" \
			'.properties.spec.additionalProperties = false |
			. + {"x-agentops-source-chart": $source}' >"${output}"
done

if [[ ${mode} == --write ]]; then
	mkdir -p "${schema_dir}"
	for schema in "${work_dir}"/*.json; do
		install -m 0644 "${schema}" "${schema_dir}/$(basename "${schema}")"
	done
	printf 'updated pinned kagent schemas from %s\n' "${chart_ref}"
	exit 0
fi

for schema in "${work_dir}"/*.json; do
	committed="${schema_dir}/$(basename "${schema}")"
	if [[ ! -f ${committed} ]] || ! cmp -s "${schema}" "${committed}"; then
		fail "${committed}: stale; run $0 --write after reviewing ${chart_ref}"
	fi
done
printf 'pinned kagent schemas match %s\n' "${chart_ref}"
