#!/usr/bin/env bash

# Resolve the cloud coordinates in a rendered GKE manifest. OpenTofu outputs
# are the default source after apply; explicit environment variables keep the
# renderer testable before any cloud resources exist.

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../scripts/lib.sh"

require_cmd kubectl platform

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${repo_dir}" || exit
template="${1:-}"
project_id="${GCP_PROJECT_ID:-}"
cluster_dns_ip="${GKE_CLUSTER_DNS_IP:-}"

if [[ -z ${project_id} || -z ${cluster_dns_ip} ]]; then
	require_cmd tofu gcp
	project_id="${project_id:-$(tofu -chdir=infra/gcp output -raw project_id)}"
	cluster_dns_ip="${cluster_dns_ip:-$(tofu -chdir=infra/gcp output -raw cluster_dns_ip)}"
fi

[[ ${project_id} =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]] ||
	fail "GCP_PROJECT_ID must be a valid Google Cloud project ID"
[[ ${cluster_dns_ip} =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] ||
	fail "GKE_CLUSTER_DNS_IP must be an IPv4 address"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-gke-render.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT
template_file="${tmp_dir}/template.yaml"
rendered_file="${tmp_dir}/rendered.yaml"

case ${template} in
"")
	kubectl kustomize infra/k8s/overlays/gke >"${template_file}"
	;;
-)
	cat >"${template_file}"
	;;
*)
	[[ -f ${template} ]] || fail "GKE manifest template does not exist: ${template}"
	cp "${template}" "${template_file}"
	;;
esac

sed \
	-e "s/__GCP_PROJECT_ID__/${project_id}/g" \
	-e "s/__GKE_CLUSTER_DNS_IP__/${cluster_dns_ip}/g" \
	"${template_file}" >"${rendered_file}"

# grep, not rg: this guard is the last thing between an unrendered placeholder and a
# live cluster, and `set -e` does not abort on a command that fails inside an `if`
# condition — so a missing binary would exit 127, read as "no match", and let the
# manifest through. grep is on every supported host; ripgrep is an installed tool.
if grep -Eq '__[A-Z0-9_]+__' "${rendered_file}"; then
	fail "rendered GKE manifest still contains unresolved placeholders"
fi

cat "${rendered_file}"
