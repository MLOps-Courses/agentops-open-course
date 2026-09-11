#!/usr/bin/env bash

# Build, render, validate, and apply the GKE workload bundle after an explicitly
# approved OpenTofu apply. The exact kubectl context check prevents a valid
# bundle from reaching the wrong cluster.

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

for command_name in docker git helmfile jq kubeconform kubectl skaffold tofu; do
	require_cmd "${command_name}" platform
done
require_host_cmd gcloud "install the Google Cloud SDK from a reviewed host package source"

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${repo_dir}" || exit
[[ -x tools/bin/source-identity ]] ||
	fail "tools/bin/source-identity is missing; run mise run install first"
source_identity_json="$(tools/bin/source-identity --root . --mode release)"
source_commit="$(jq -er '.revision' <<<"${source_identity_json}")"
[[ -z ${AGENT_SOURCE_COMMIT:-} || ${AGENT_SOURCE_COMMIT} == "${source_commit}" ]] ||
	fail "AGENT_SOURCE_COMMIT does not match the resolved clean source"
export AGENT_BUILD_MODE=release
export AGENT_SOURCE_COMMIT="${source_commit}"
export AGENT_SOURCE_REVISION="${source_commit}"
export AGENT_IMAGE_TAG="${source_commit}"
export AGENT_SOURCE_TREE_DIGEST
export AGENT_SOURCE_DIRTY=false
export OCI_CREATED
export OCI_VERSION
AGENT_SOURCE_TREE_DIGEST="$(jq -er '.tree_digest' <<<"${source_identity_json}")"
OCI_CREATED="$(git show -s --format=%cI HEAD)"
OCI_VERSION="$(tr -d '\n' <VERSION)"

project_id="$(tofu -chdir=infra/gcp output -raw project_id)"
cluster_name="$(tofu -chdir=infra/gcp output -raw cluster_name)"
cluster_zone="$(tofu -chdir=infra/gcp output -raw cluster_zone)"
repository="$(tofu -chdir=infra/gcp output -raw artifact_registry_repository)"
registry_host="${repository%%/*}"
[[ ${registry_host} =~ ^[a-z0-9-]+-docker\.pkg\.dev$ ]] ||
	fail "Artifact Registry host is invalid: ${registry_host}"
expected_context="gke_${project_id}_${cluster_zone}_${cluster_name}"
current_context="$(kubectl config current-context)"

[[ ${current_context} == "${expected_context}" ]] ||
	fail "kubectl context is ${current_context}; expected ${expected_context}"

docker_config="$(mktemp -d "${TMPDIR:-/tmp}/agentops-gke-docker.XXXXXX")"
cleanup() {
	rm -rf -- "${docker_config}"
}
trap cleanup EXIT
export DOCKER_CONFIG="${docker_config}"
gcloud auth configure-docker "${registry_host}" --quiet >/dev/null

mkdir -p .agents/tmp
artifacts=".agents/tmp/gke-artifacts.json"
template=".agents/tmp/gke-template.yaml"
manifest=".agents/tmp/gke.yaml"

(
	cd infra || exit
	SKAFFOLD_DEFAULT_REPO="${repository}" skaffold build \
		--filename skaffold.yaml \
		--profile gke \
		--file-output "../${artifacts}"
	skaffold render \
		--filename skaffold.yaml \
		--profile gke \
		--build-artifacts "../${artifacts}" \
		--output "../${template}"
)
infra/scripts/render-gke.sh "${template}" >"${manifest}"
kubeconform \
	-strict \
	-kubernetes-version 1.36.0 \
	-schema-location default \
	-schema-location 'infra/kagent/schemas/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json' \
	-summary \
	"${manifest}"

kubectl --context "${expected_context}" apply -f infra/k8s/base/namespace.yaml
helmfile \
	--file infra/helmfile.yaml \
	--kube-context "${expected_context}" \
	apply \
	--skip-diff-on-install
kubectl --context "${expected_context}" apply -f "${manifest}"
