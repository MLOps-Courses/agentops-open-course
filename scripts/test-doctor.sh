#!/usr/bin/env bash

# shellcheck source=scripts/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

doctor="$(dirname "${BASH_SOURCE[0]}")/doctor.sh"
doctor_path="$(realpath "${doctor}")"
repo_dir="$(dirname "$(dirname "${doctor_path}")")"
tmp_dir="$(mktemp -d)"
trap 'rm -r -- "${tmp_dir}"' EXIT

"${doctor}" --requirements base >"${tmp_dir}/base"
"${doctor}" --requirements gateway >"${tmp_dir}/gateway"
"${doctor}" --requirements gcp >"${tmp_dir}/gcp"

grep -Fqx $'sqlite3\tmise run install' "${tmp_dir}/base"
# rg is a base-tier tool, and the remedy it names has to be able to supply it: check:licenses:core
# greps the font license with `rg`, and macOS ships none. When the base tier omitted it, the base
# doctor reported ready and check:core then failed on a tool no learner had been told to install —
# so both halves of that contract are asserted here rather than left to a hand audit.
grep -Fqx $'rg\tmise run install' "${tmp_dir}/base" ||
	fail "the base doctor tier must require rg: check:licenses:core greps the font license with it"
# The installer line itself, not the comment above it: a section that only explains why ripgrep
# belongs here would satisfy a plain substring match while installing nothing.
sed -n '/^\[tasks\."install:tools:core"\]/,/^\[tasks\./p' "${repo_dir}/mise.toml" |
	grep -Eq '^ *"mise install .* ripgrep ' ||
	fail "install:tools:core must install ripgrep: it is the remedy the base doctor prints for rg"
grep -Fqx $'git\tinstall Git from a reviewed host package source' "${tmp_dir}/base"
grep -Fqx $'install\tinstall it from a reviewed host package source' "${tmp_dir}/base"
grep -Fqx $'yq\tmise run install:platform' "${tmp_dir}/gateway"
grep -Fqx $'docker\tfollow 1.2. Container Engine to install a supported container engine' "${tmp_dir}/gateway"
grep -Fqx $'gcloud\tinstall the Google Cloud CLI from a reviewed host package source' "${tmp_dir}/gcp"
grep -Fqx $'gke-gcloud-auth-plugin\tinstall it from the same reviewed Cloud SDK or host package source' "${tmp_dir}/gcp"

if rg -F $'docker\tmise run install:platform' "${tmp_dir}/gateway" >/dev/null; then
	fail "doctor mapped the host container engine to the platform installer"
fi
if rg -F $'gke-gcloud-auth-plugin\tmise run install:gcp' "${tmp_dir}/gcp" >/dev/null; then
	fail "doctor mapped the host GKE auth plugin to a repository installer"
fi

mkdir -p "${tmp_dir}/bin"
cat >"${tmp_dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat >"${tmp_dir}/bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ $* == "plugin list" ]] || exit 64
printf 'NAME VERSION\ndiff 3.15.10\n'
EOF
cat >"${tmp_dir}/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ -z ${FAKE_GCLOUD_LOG:-} ]] || printf '%s\n' "$*" >>"${FAKE_GCLOUD_LOG}"

case "$*" in
"config get-value project"*)
	printf '%s\n' "${FAKE_GCLOUD_AMBIENT_PROJECT:-(unset)}"
	;;
"projects describe "*)
	project_id="$3"
	if [[ $* == *"--format=json"* ]]; then
		jq -cn --arg id "${project_id}" '{projectId:$id,projectNumber:"123456789012"}'
	else
		printf '%s\n' "${project_id}"
	fi
	;;
"billing projects describe "*)
	printf 'True\n'
	;;
"auth list "*)
	printf 'operator@example.test\n'
	;;
"auth application-default print-access-token")
	printf 'synthetic-token-never-logged\n'
	;;
*)
	printf 'unexpected fake gcloud call: %s\n' "$*" >&2
	exit 64
	;;
esac
EOF
# The GKE auth plugin ships with the Cloud SDK, not with this repository, so a host
# without it would otherwise fail this test rather than the GCP path it is meant to
# exercise. doctor.sh only probes for the binary's presence, so an exit-0 stub is a
# faithful stand-in and keeps the pre-push gate hermetic.
cat >"${tmp_dir}/bin/gke-gcloud-auth-plugin" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${tmp_dir}/bin/docker" "${tmp_dir}/bin/helm" "${tmp_dir}/bin/gcloud" "${tmp_dir}/bin/gke-gcloud-auth-plugin"
export FAKE_GCLOUD_LOG="${tmp_dir}/gcloud.log"

run_gcp_doctor() (
	cd "${repo_dir}"
	PATH="${tmp_dir}/bin:${PATH}" "${doctor_path}" gcp
)

if (
	unset GCP_PROJECT_ID
	export FAKE_GCLOUD_AMBIENT_PROJECT=ambient-project
	run_gcp_doctor
) >"${tmp_dir}/missing-project.out" 2>"${tmp_dir}/missing-project.err"; then
	fail "doctor:gcp accepted an ambient project without explicit approval"
fi
[[ ! -e ${FAKE_GCLOUD_LOG} ]] || fail "doctor:gcp inspected gcloud before requiring explicit approval"
rm -f -- "${FAKE_GCLOUD_LOG}"
if GCP_PROJECT_ID=approved-project FAKE_GCLOUD_AMBIENT_PROJECT=other-project \
	run_gcp_doctor >"${tmp_dir}/mismatched-project.out" 2>"${tmp_dir}/mismatched-project.err"; then
	fail "doctor:gcp accepted an ambient project that disagreed with approval"
fi
if rg -F 'projects describe' "${FAKE_GCLOUD_LOG}" >/dev/null; then
	fail "doctor:gcp inspected a project after detecting an ambient mismatch"
fi
rm -f -- "${FAKE_GCLOUD_LOG}"
GCP_PROJECT_ID=approved-project FAKE_GCLOUD_AMBIENT_PROJECT=approved-project \
	run_gcp_doctor >"${tmp_dir}/approved-project.out"
grep -Fq 'project approved-project is active and billing-enabled' "${tmp_dir}/approved-project.out"

# doctor:model must not accept a provider that lists tags but cannot infer. A stale
# `ollama serve` whose install tree had been deleted did exactly that, and the profile
# reported ready while every model call failed in under a second.
cat >"${tmp_dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url="${*: -1}"
case "${url}" in
*/api/version) printf '{"version":"0.32.9"}\n' ;;
*/api/tags) printf '{"models":[{"name":"qwen3:4b-instruct"}]}\n' ;;
*/v1/responses)
	[[ ${FAKE_OLLAMA_CAN_INFER:-1} == 1 ]] || exit 7
	printf '{"status":"completed"}\n'
	;;
*)
	printf 'unexpected fake curl call: %s\n' "$*" >&2
	exit 64
	;;
esac
EOF
chmod +x "${tmp_dir}/bin/curl"
# `doctor model` requires the Ollama binary on the host, and CI runners have none. The
# profile only probes that the command exists — the version it reports comes from the
# fake curl above — so a presence stub first on PATH makes the result identical on a
# workstation that runs models and on a runner that never will.
cat >"${tmp_dir}/bin/ollama" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
chmod +x "${tmp_dir}/bin/ollama"
run_model_doctor() (
	cd "${repo_dir}"
	PATH="${tmp_dir}/bin:${PATH}" "${doctor_path}" model
)

if FAKE_OLLAMA_CAN_INFER=0 run_model_doctor >"${tmp_dir}/dead-model.out" 2>"${tmp_dir}/dead-model.err"; then
	fail "doctor:model accepted a provider that answers /api/tags but cannot infer"
fi
grep -Fq 'cannot infer' "${tmp_dir}/dead-model.err"
run_model_doctor >"${tmp_dir}/live-model.out"
grep -Eq '^inference  ok in [0-9]+s ' "${tmp_dir}/live-model.out" ||
	fail "doctor:model did not report a measured inference time"
