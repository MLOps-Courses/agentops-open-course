#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly script_dir
repo_root="$(cd -- "${script_dir}/../.." && pwd)"
readonly repo_root
readonly host_config_dir="${repo_root}/infra/agentgateway/host"
readonly image="cr.agentgateway.dev/agentgateway:v1.3.1@sha256:c3ce7b75da90fef70239befcc1c3adc05152d7b9dd21fcb8351178026a2c4381"
readonly managed_label="dev.fmind.agentops.host-gateway"
readonly relay_script="${script_dir}/loopback-relay.py"

readonly container_name="${AGENTOPS_GATEWAY_CONTAINER:-agentops-host-gateway}"
readonly mcp_port="${AGENTOPS_GATEWAY_MCP_PORT:-3000}"
readonly a2a_port="${AGENTOPS_GATEWAY_A2A_PORT:-3001}"
readonly model_port="${AGENTOPS_GATEWAY_MODEL_PORT:-4000}"
readonly metrics_port="${AGENTOPS_GATEWAY_METRICS_PORT:-15020}"
readonly readiness_port="${AGENTOPS_GATEWAY_READINESS_PORT:-15021}"
readonly mcp_upstream_port="${AGENTOPS_MCP_UPSTREAM_PORT:-8000}"
readonly a2a_upstream_port="${AGENTOPS_A2A_UPSTREAM_PORT:-8080}"
readonly model_upstream_port="${AGENTOPS_MODEL_UPSTREAM_PORT:-11434}"
canonical_config_input="${AGENTOPS_GATEWAY_CONFIG:-${host_config_dir}/config.yaml}"
if [[ ! -f "${canonical_config_input}" && -f "${host_config_dir}/${canonical_config_input}" ]]; then
	canonical_config_input="${host_config_dir}/${canonical_config_input}"
fi
readonly canonical_config_input
auth_dir_input="${AGENTOPS_GATEWAY_AUTH_DIR:-${host_config_dir}/auth}"
if [[ ! -d "${auth_dir_input}" && -d "${host_config_dir}/${auth_dir_input}" ]]; then
	auth_dir_input="${host_config_dir}/${auth_dir_input}"
fi
readonly auth_dir_input
readonly runtime_root="${AGENTOPS_GATEWAY_RUNTIME_DIR:-${XDG_RUNTIME_DIR:-${TMPDIR:-/tmp}}/agentops-open-course}"
readonly relay_mode="${AGENTOPS_GATEWAY_LOOPBACK_RELAY:-auto}"
readonly relay_python="${AGENTOPS_GATEWAY_PYTHON:-${repo_root}/agents/python/.venv/bin/python}"

usage() {
	cat <<'EOF'
Usage: infra/scripts/gateway-host.sh COMMAND

Run the canonical host agentgateway profile in one hardened, scoped container.

Commands:
  run      Run in the foreground and remove the container on exit.
  start    Start a detached container.
  stop     Stop and remove only the managed container with the configured name.
  status   Show the managed container status.
  logs     Follow logs from the managed container.
  render   Render the canonical host config for the container network.
  validate Validate the rendered config inside the pinned container.
  args     Print the exact foreground Docker arguments, one per line.

Environment:
  AGENTOPS_GATEWAY_CONTAINER       Container name (agentops-host-gateway).
  AGENTOPS_GATEWAY_MCP_PORT        Loopback MCP port (3000).
  AGENTOPS_GATEWAY_A2A_PORT        Loopback A2A port (3001).
  AGENTOPS_GATEWAY_MODEL_PORT      Loopback model port (4000).
  AGENTOPS_GATEWAY_METRICS_PORT    Loopback metrics port (15020).
  AGENTOPS_GATEWAY_READINESS_PORT  Loopback readiness port (15021).
  AGENTOPS_GATEWAY_CONFIG          Canonical config path or host-config basename.
  AGENTOPS_GATEWAY_AUTH_DIR        Source TLS/JWKS directory for secured configs.
  AGENTOPS_GATEWAY_LOOPBACK_RELAY  auto (default), on, or off.
  AGENTOPS_GATEWAY_PYTHON          Installed Python used by the Linux relay.
  AGENTOPS_MCP_UPSTREAM_PORT       Host MCP upstream port (8000).
  AGENTOPS_A2A_UPSTREAM_PORT       Host A2A upstream port (8080).
  AGENTOPS_MODEL_UPSTREAM_PORT     Host model upstream port (11434).
EOF
}

die() {
	echo "gateway-host: $*" >&2
	exit 1
}

validate_port() {
	local name="$1"
	local value="$2"
	local number

	[[ "${value}" =~ ^[0-9]+$ ]] || die "${name} must be an integer, got '${value}'"
	number=$((10#${value}))
	((number >= 1 && number <= 65535)) || die "${name} must be between 1 and 65535, got '${value}'"
}

config_needs_auth() {
	yq -e '
		[.binds[].listeners[] | select(has("tls"))] | length > 0
	' "${canonical_config_input}" >/dev/null 2>&1
}

validate_inputs() {
	[[ "${container_name}" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]*$ ]] ||
		die "AGENTOPS_GATEWAY_CONTAINER is not a valid Docker name: '${container_name}'"
	[[ -f "${canonical_config_input}" ]] || die "canonical config not found: ${canonical_config_input}"
	command -v yq >/dev/null || die "yq is required"
	case "${relay_mode}" in
	auto | on | off) ;;
	*) die "AGENTOPS_GATEWAY_LOOPBACK_RELAY must be auto, on, or off" ;;
	esac

	validate_port AGENTOPS_GATEWAY_MCP_PORT "${mcp_port}"
	validate_port AGENTOPS_GATEWAY_A2A_PORT "${a2a_port}"
	validate_port AGENTOPS_GATEWAY_MODEL_PORT "${model_port}"
	validate_port AGENTOPS_GATEWAY_METRICS_PORT "${metrics_port}"
	validate_port AGENTOPS_GATEWAY_READINESS_PORT "${readiness_port}"
	validate_port AGENTOPS_MCP_UPSTREAM_PORT "${mcp_upstream_port}"
	validate_port AGENTOPS_A2A_UPSTREAM_PORT "${a2a_upstream_port}"
	validate_port AGENTOPS_MODEL_UPSTREAM_PORT "${model_upstream_port}"

	yq -e '
		([.binds[] | select(.port == 3000)] | length == 1) and
		([.binds[] | select(.port == 3001)] | length == 1) and
		([.binds[] | select(.port == 4000)] | length == 1)
	' "${canonical_config_input}" >/dev/null ||
		die "canonical config must contain exactly one MCP, A2A, and model bind"

	if config_needs_auth; then
		[[ -d "${auth_dir_input}" ]] || die "secured config requires auth directory: ${auth_dir_input}"
		for auth_file in tls-cert.pem tls-key.pem jwks.json; do
			[[ -r "${auth_dir_input}/${auth_file}" ]] ||
				die "secured config requires readable auth file: ${auth_dir_input}/${auth_file}"
		done
	fi
}

render_base_config() {
	MCP_UPSTREAM_PORT="${mcp_upstream_port}" \
		A2A_UPSTREAM_PORT="${a2a_upstream_port}" \
		MODEL_UPSTREAM_PORT="${model_upstream_port}" \
		yq '
			(
				.binds[] |
				select(.port == 3000) |
				.listeners[].routes[].backends[].mcp.targets[].mcp.host
			) = ("http://host.docker.internal:" + strenv(MCP_UPSTREAM_PORT) + "/mcp") |
			(
				.binds[] |
				select(.port == 3001) |
				.listeners[].routes[].backends[].host
			) = ("host.docker.internal:" + strenv(A2A_UPSTREAM_PORT)) |
			(
				.binds[] |
				select(.port == 4000) |
				.listeners[].routes[].backends[].ai.hostOverride
			) = ("host.docker.internal:" + strenv(MODEL_UPSTREAM_PORT)) |
			.config.statsAddr = "0.0.0.0:15020" |
			.config.readinessAddr = "0.0.0.0:15021" |
			.config.adminAddr = "off"
		' "${canonical_config_input}"
}

render_config() {
	if ! config_needs_auth; then
		render_base_config
		return
	fi

	render_base_config |
		yq '
			(
				.binds[].listeners[] |
				select(.tls.cert != null) |
				.tls.cert
			) = "/etc/agentgateway/auth/tls-cert.pem" |
			(
				.binds[].listeners[] |
				select(.tls.key != null) |
				.tls.key
			) = "/etc/agentgateway/auth/tls-key.pem" |
			(
				.binds[].listeners[].routes[] |
				select(.policies.jwtAuth.jwks.file != null) |
				.policies.jwtAuth.jwks.file
			) = "/etc/agentgateway/auth/jwks.json"
		' -
}

write_runtime_config() {
	local directory="$1"
	local auth_directory="${directory}/auth"
	local auth_file

	mkdir -p -- "${directory}"
	chmod 0700 -- "${directory}"
	render_config >"${directory}/config.yaml"
	chmod 0444 -- "${directory}/config.yaml"

	if config_needs_auth; then
		mkdir -p -- "${auth_directory}"
		chmod 0700 -- "${auth_directory}"
		for auth_file in tls-cert.pem tls-key.pem jwks.json; do
			cp "${auth_dir_input}/${auth_file}" "${auth_directory}/${auth_file}"
			chmod 0444 -- "${auth_directory}/${auth_file}"
		done
		# The parent runtime directory remains private to the invoking user.
		# This mount root must be traversable by the container's non-root UID.
		chmod 0555 -- "${auth_directory}"
	fi
}

require_docker() {
	command -v docker >/dev/null || die "docker is required"
	docker info >/dev/null 2>&1 || die "Docker daemon is unavailable"
}

container_exists() {
	docker container inspect "${container_name}" >/dev/null 2>&1
}

container_is_managed() {
	local label

	label="$(docker container inspect --format "{{ index .Config.Labels \"${managed_label}\" }}" "${container_name}")"
	[[ "${label}" == "true" ]]
}

assert_container_absent() {
	if container_exists; then
		die "container '${container_name}' already exists; choose another name or inspect it before removal"
	fi
}

stop_managed_container() {
	local running

	if ! container_exists; then
		return
	fi
	container_is_managed ||
		die "refusing to stop unowned container '${container_name}'; required label ${managed_label}=true is absent"

	running="$(docker container inspect --format '{{.State.Running}}' "${container_name}")"
	if [[ "${running}" == "true" ]]; then
		docker container stop --time 10 "${container_name}" >/dev/null
	fi
	if container_exists; then
		docker container rm "${container_name}" >/dev/null
	fi
}

declare -a docker_args

build_docker_args() {
	local config_path="$1"
	local lifecycle="$2"
	local config_directory

	config_directory="$(dirname -- "${config_path}")"

	docker_args=(
		run
		--pull missing
		--user 65532:65532
		--read-only
		--cap-drop ALL
		--security-opt no-new-privileges=true
		--tmpfs "/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777"
		--add-host host.docker.internal:host-gateway
	)
	if [[ "${lifecycle}" != "validate" ]]; then
		docker_args+=(
			--name "${container_name}"
			--label "${managed_label}=true"
			--publish "127.0.0.1:${mcp_port}:3000"
			--publish "127.0.0.1:${a2a_port}:3001"
			--publish "127.0.0.1:${model_port}:4000"
			--publish "127.0.0.1:${metrics_port}:15020"
			--publish "127.0.0.1:${readiness_port}:15021"
		)
	fi
	docker_args+=(
		--mount "type=bind,src=${config_path},dst=/etc/agentgateway/config.yaml,readonly"
	)
	if [[ -d "${config_directory}/auth" ]]; then
		docker_args+=(
			--mount "type=bind,src=${config_directory}/auth,dst=/etc/agentgateway/auth,readonly"
		)
	fi
	case "${lifecycle}" in
	run)
		docker_args+=(--rm)
		;;
	start)
		docker_args+=(--detach)
		;;
	validate)
		docker_args+=(--rm --network none)
		;;
	*)
		die "unsupported lifecycle '${lifecycle}'"
		;;
	esac
	docker_args+=("${image}")
	if [[ "${lifecycle}" == "validate" ]]; then
		docker_args+=(--validate-only)
	fi
	docker_args+=(-f /etc/agentgateway/config.yaml)
}

remove_runtime_dir() {
	local directory="$1"

	if [[ -d "${directory}/auth" ]]; then
		chmod 0700 -- "${directory}/auth"
	fi
	rm -rf -- "${directory}"
}

loopback_relay_required() {
	local docker_os
	local kernel

	case "${relay_mode}" in
	on)
		return 0
		;;
	off)
		return 1
		;;
	auto) ;;
	*)
		die "unsupported loopback relay mode '${relay_mode}'"
		;;
	esac

	kernel="$(uname -s)"
	[[ "${kernel}" == "Linux" ]] || return 1
	docker_os="$(docker info --format '{{.OperatingSystem}}')"
	[[ "${docker_os}" != *"Docker Desktop"* ]]
}

docker_bridge_gateway() {
	local gateway

	gateway="$(docker network inspect bridge --format '{{(index .IPAM.Config 0).Gateway}}')"
	[[ "${gateway}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
		die "could not discover Docker's IPv4 bridge gateway"
	echo "${gateway}"
}

stop_loopback_relay() {
	local run_dir="$1"
	local relay_dir="${run_dir}/relay"
	local pid
	local token
	local command
	local state
	local attempt

	[[ -f "${relay_dir}/pid" && -f "${relay_dir}/token" ]] || return 0
	read -r pid <"${relay_dir}/pid"
	read -r token <"${relay_dir}/token"
	[[ "${pid}" =~ ^[0-9]+$ ]] || die "invalid loopback relay PID metadata"

	if kill -0 "${pid}" >/dev/null 2>&1; then
		command="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
		if [[ "${command}" != *"${relay_script}"* || "${command}" != *"--token ${token}"* ]]; then
			die "refusing to stop PID ${pid}: it is not this wrapper's loopback relay"
		fi
		kill -TERM "${pid}" >/dev/null 2>&1 || true
		for ((attempt = 0; attempt < 20; attempt += 1)); do
			if ! kill -0 "${pid}" >/dev/null 2>&1; then
				break
			fi
			state="$(ps -p "${pid}" -o stat= 2>/dev/null || true)"
			[[ "${state}" == Z* ]] && break
			sleep 0.1
		done
		if kill -0 "${pid}" >/dev/null 2>&1; then
			state="$(ps -p "${pid}" -o stat= 2>/dev/null || true)"
			if [[ "${state}" != Z* ]]; then
				kill -KILL "${pid}" >/dev/null 2>&1 || true
			fi
		fi
		wait "${pid}" 2>/dev/null || true
	fi
}

start_loopback_relay() {
	local run_dir="$1"
	local relay_dir="${run_dir}/relay"
	local listen_host
	local token
	local pid
	local attempt

	loopback_relay_required || return 0
	[[ -x "${relay_python}" ]] ||
		die "Linux loopback relay requires the installed agent Python; run 'mise run install' first"
	[[ -f "${relay_script}" ]] || die "loopback relay helper not found: ${relay_script}"

	listen_host="$(docker_bridge_gateway)"
	token="${container_name}-$$-${RANDOM}-${RANDOM}"
	mkdir -p -- "${relay_dir}"
	chmod 0700 -- "${relay_dir}"
	printf '%s\n' "${token}" >"${relay_dir}/token"
	printf '%s\n' "${listen_host}" >"${relay_dir}/listen-host"

	nohup "${relay_python}" "${relay_script}" \
		--listen-host "${listen_host}" \
		--target-host 127.0.0.1 \
		--port "${mcp_upstream_port}" \
		--port "${a2a_upstream_port}" \
		--port "${model_upstream_port}" \
		--port "${metrics_port}" \
		--ready-file "${relay_dir}/ready" \
		--token "${token}" \
		</dev/null >"${relay_dir}/relay.log" 2>&1 &
	pid=$!
	printf '%s\n' "${pid}" >"${relay_dir}/pid"

	for ((attempt = 0; attempt < 100; attempt += 1)); do
		[[ -f "${relay_dir}/ready" ]] && return
		if ! kill -0 "${pid}" >/dev/null 2>&1; then
			tail -n 40 "${relay_dir}/relay.log" >&2 || true
			die "loopback relay exited before becoming ready"
		fi
		sleep 0.05
	done

	stop_loopback_relay "${run_dir}"
	tail -n 40 "${relay_dir}/relay.log" >&2 || true
	die "loopback relay did not become ready"
}

run_foreground() {
	local run_dir="${runtime_root}/${container_name}"

	require_docker
	assert_container_absent
	remove_runtime_dir "${run_dir}"
	write_runtime_config "${run_dir}"
	build_docker_args "${run_dir}/config.yaml" run

	cleanup_foreground() {
		if container_exists && container_is_managed; then
			docker container stop --time 10 "${container_name}" >/dev/null 2>&1 || true
		fi
		stop_loopback_relay "${run_dir}"
		remove_runtime_dir "${run_dir}"
	}
	trap cleanup_foreground EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	start_loopback_relay "${run_dir}"
	docker "${docker_args[@]}"
}

start_detached() {
	local run_dir="${runtime_root}/${container_name}"

	require_docker
	assert_container_absent
	remove_runtime_dir "${run_dir}"
	write_runtime_config "${run_dir}"
	build_docker_args "${run_dir}/config.yaml" start
	cleanup_failed_start() {
		stop_loopback_relay "${run_dir}"
		remove_runtime_dir "${run_dir}"
	}
	trap cleanup_failed_start EXIT
	start_loopback_relay "${run_dir}"
	if ! docker "${docker_args[@]}"; then
		return 1
	fi
	trap - EXIT
}

stop_detached() {
	local run_dir="${runtime_root}/${container_name}"

	require_docker
	stop_managed_container
	stop_loopback_relay "${run_dir}"
	remove_runtime_dir "${run_dir}"
}

show_status() {
	require_docker
	container_exists || die "managed container '${container_name}' does not exist"
	container_is_managed ||
		die "container '${container_name}' exists but is not owned by this wrapper"
	docker container inspect --format 'name={{.Name}} status={{.State.Status}} image={{.Config.Image}}' "${container_name}"
}

show_logs() {
	require_docker
	container_exists || die "managed container '${container_name}' does not exist"
	container_is_managed ||
		die "container '${container_name}' exists but is not owned by this wrapper"
	docker container logs --follow "${container_name}"
}

validate_config() {
	local validate_dir
	local result=0

	require_docker
	validate_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-gateway-validate.XXXXXX")"
	write_runtime_config "${validate_dir}"
	build_docker_args "${validate_dir}/config.yaml" validate
	docker "${docker_args[@]}" || result=$?
	remove_runtime_dir "${validate_dir}"
	return "${result}"
}

print_args() {
	local args_dir

	args_dir="$(mktemp -d "${TMPDIR:-/tmp}/agentops-gateway-args.XXXXXX")"
	write_runtime_config "${args_dir}"
	build_docker_args "${args_dir}/config.yaml" run
	printf '%s\n' "${docker_args[@]}"
	remove_runtime_dir "${args_dir}"
}

main() {
	local command="${1:-}"

	[[ $# -le 1 ]] || {
		usage >&2
		exit 2
	}
	validate_inputs

	case "${command}" in
	run)
		run_foreground
		;;
	start)
		start_detached
		;;
	stop)
		stop_detached
		;;
	status)
		show_status
		;;
	logs)
		show_logs
		;;
	render)
		render_config
		;;
	validate)
		validate_config
		;;
	args)
		print_args
		;;
	-h | --help | help)
		usage
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
}

main "$@"
