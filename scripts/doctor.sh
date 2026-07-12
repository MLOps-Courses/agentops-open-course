#!/usr/bin/env bash

set -Eeuo pipefail

required=(
	git
	docker
	uv
	dprint
	k3d
	kubectl
	helm
	helmfile
	skaffold
	kustomize
	kubeconform
	kube-linter
	tofu
	trivy
	agentgateway
	lychee
	jq
	gh
	yq
)

missing=0
for command_name in "${required[@]}"; do
	if ! command -v "${command_name}" >/dev/null 2>&1; then
		printf 'missing: %s\n' "${command_name}" >&2
		missing=1
	fi
done

if ((missing)); then
	printf '\nRun mise install after installing Git and Docker.\n' >&2
	exit 1
fi

if ! docker info >/dev/null 2>&1; then
	printf 'docker: daemon is unavailable\n' >&2
	exit 1
fi
docker compose version >/dev/null
if ! helm plugin list | rg -q '^diff[[:space:]]+3\.15\.10'; then
	printf 'helm: helm-diff 3.15.10 is missing; run mise run install\n' >&2
	exit 1
fi

printf 'toolchain: ready\n'
printf 'docker:    ready\n'

if [[ -f .env ]]; then
	printf 'env:       .env loaded\n'
else
	printf 'env:       optional .env is absent (copy .env.example before live-model work)\n'
fi

if command -v ollama >/dev/null 2>&1; then
	printf 'ollama:    available for the local Qwen3 path\n'
else
	printf 'ollama:    optional; install it for the fully local model path\n'
fi

if command -v gcloud >/dev/null 2>&1; then
	printf 'gcloud:    available for the optional GKE path\n'
else
	printf 'gcloud:    optional; install it only for the GKE chapters\n'
fi

context=$(kubectl config current-context 2>/dev/null || true)
if [[ "${context}" == "k3d-local" ]]; then
	printf 'cluster:   k3d-local selected\n'
elif [[ -n "${context}" ]]; then
	printf 'cluster:   %s selected (local tasks require k3d-local)\n' "${context}"
else
	printf 'cluster:   not created yet\n'
fi
