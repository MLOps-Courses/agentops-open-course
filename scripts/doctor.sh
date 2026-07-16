#!/usr/bin/env bash

set -Eeuo pipefail

profile=${1:-base}
case "${profile}" in
base)
	required=(git uv dprint)
	;;
model)
	required=(git uv dprint curl jq ollama)
	;;
gateway)
	required=(git uv dprint curl docker jq yq)
	;;
platform)
	required=(
		git
		uv
		dprint
		curl
		docker
		jq
		rg
		yq
		k3d
		kubectl
		helm
		helmfile
		skaffold
		kustomize
		kubeconform
		kube-linter
		agentgateway
	)
	;;
gcp)
	required=(
		git
		uv
		dprint
		curl
		docker
		jq
		rg
		yq
		k3d
		kubectl
		helm
		helmfile
		skaffold
		kustomize
		kubeconform
		kube-linter
		agentgateway
		tofu
		tflint
		gcloud
	)
	;;
*)
	printf 'usage: %s {base|model|gateway|platform|gcp}\n' "$0" >&2
	exit 2
	;;
esac

missing=0
for command_name in "${required[@]}"; do
	if ! command -v "${command_name}" >/dev/null 2>&1; then
		printf 'missing: %s\n' "${command_name}" >&2
		missing=1
	fi
done

if ((missing)); then
	printf '\nInstall the missing prerequisites for the %s learning path.\n' "${profile}" >&2
	exit 1
fi

for python_environment in .venv/bin/python agents/python/.venv/bin/python; do
	if [[ ! -x ${python_environment} ]]; then
		printf '%s missing; run mise run install\n' "${python_environment}" >&2
		exit 1
	fi
done

printf '%-10s ready\n' "${profile}"

if [[ -f .env ]]; then
	printf 'env        .env available to explicit live/config tasks\n'
else
	printf 'env        optional .env is absent\n'
fi

if [[ ${profile} == model ]]; then
	if ! curl --fail --silent --show-error http://127.0.0.1:11434/api/tags |
		jq -e '.models[]?.name | startswith("qwen3:4b")' >/dev/null; then
		printf 'ollama     start Ollama and run: ollama pull qwen3:4b\n' >&2
		exit 1
	fi
	printf 'ollama     qwen3:4b ready on 127.0.0.1:11434\n'
fi

case "${profile}" in
gateway | platform | gcp)
	[[ -x infra/scripts/gateway-host.sh ]] || {
		printf 'gateway    wrapper is not executable\n' >&2
		exit 1
	}
	if ! docker info >/dev/null 2>&1; then
		printf 'docker     daemon is unavailable\n' >&2
		exit 1
	fi
	docker compose version >/dev/null
	printf 'docker     ready\n'
	;;
*)
	;;
esac

case "${profile}" in
platform | gcp)
	if ! helm plugin list | rg -q '^diff[[:space:]]+3\.15\.10'; then
		printf 'helm       helm-diff 3.15.10 is missing; run mise run install\n' >&2
		exit 1
	fi
	printf 'helm       helm-diff 3.15.10 ready\n'

	context=$(kubectl config current-context 2>/dev/null || true)
	if [[ ${context} == "k3d-local" ]]; then
		printf 'cluster    k3d-local selected\n'
	elif [[ -n ${context} ]]; then
		printf 'cluster    %s selected; local tasks require k3d-local\n' "${context}"
	else
		printf 'cluster    not created yet; run mise run cluster:start when needed\n'
	fi
	;;
*)
	;;
esac

if [[ ${profile} == gcp ]]; then
	project=$(gcloud config get-value project 2>/dev/null || true)
	if [[ -n ${project} ]]; then
		printf 'gcp        active project: %s\n' "${project}"
	else
		printf 'gcp        no active project; select your billing-enabled project before the optional lab\n' >&2
		exit 1
	fi
	if ! gcloud auth application-default print-access-token >/dev/null 2>&1; then
		printf 'gcp        ADC unavailable; run gcloud auth application-default login\n' >&2
		exit 1
	fi
	printf 'gcp        Application Default Credentials ready\n'
fi
