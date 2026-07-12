#!/usr/bin/env bash

set -Eeuo pipefail

if ! docker info >/dev/null 2>&1; then
	printf 'docker: daemon is unavailable\n' >&2
	exit 1
fi

clusters=$(k3d cluster list -o json)
registries=$(k3d registry list -o json)

if jq -e 'any(.[]; .name == "local")' <<<"${clusters}" >/dev/null; then
	if ! jq -e 'any(.[]; .name == "registry.localhost")' <<<"${registries}" >/dev/null; then
		printf 'k3d: cluster local exists without registry.localhost; reconcile it before continuing\n' >&2
		exit 1
	fi
	if ! jq -e '.[] | select(.name == "local") | .serversRunning == .serversCount' <<<"${clusters}" >/dev/null; then
		k3d cluster start local
	fi
else
	if jq -e 'any(.[]; .name == "registry.localhost")' <<<"${registries}" >/dev/null; then
		printf 'k3d: registry.localhost exists without cluster local; reconcile it before continuing\n' >&2
		exit 1
	fi
	k3d cluster create --config infra/k3d.yaml
fi

kubectl config use-context k3d-local >/dev/null
kubectl cluster-info >/dev/null
printf 'cluster: k3d-local is ready with registry.localhost:5050\n'
