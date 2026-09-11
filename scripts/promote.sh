#!/usr/bin/env bash

# Promotion preflight (Chapter 6.7): validate the committed eval set, current
# source behavior, and target overlay before a rollout. A guarded build/deploy
# command is emitted only when model-backed evidence also passes for one clean
# source identity. The script neither builds an image nor applies anything to a cluster.
#
#   scripts/promote.sh                 # offline preflight for the local overlay
#   scripts/promote.sh gke             # offline preflight for the gke overlay
#   scripts/promote.sh --with-model    # behavior gate + local promote command

lib_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/lib.sh
source "${lib_dir}/lib.sh"

require_cmd kubectl platform
require_cmd jq base
require_cmd skaffold platform

overlay=local
overlay_set=0
with_model=0
candidate_commit=
candidate_tree_digest=
for arg in "$@"; do
	case ${arg} in
	--with-model) with_model=1 ;;
	--*)
		printf 'promote: unknown flag %q\n' "${arg}" >&2
		exit 2
		;;
	local | gke)
		if ((overlay_set)); then
			printf 'promote: expected one overlay, got %q after %q\n' "${arg}" "${overlay}" >&2
			exit 2
		fi
		overlay=${arg}
		overlay_set=1
		;;
	*)
		printf 'promote: unknown overlay %q (expected local or gke)\n' "${arg}" >&2
		exit 2
		;;
	esac
done

if ((with_model)); then
	[[ -x tools/bin/source-identity ]] ||
		fail "promote: tools/bin/source-identity is missing; run mise run install first"
	candidate_identity="$(tools/bin/source-identity --root . --mode release)"
	candidate_commit="$(jq -er .revision <<<"${candidate_identity}")"
	candidate_tree_digest="$(jq -er .tree_digest <<<"${candidate_identity}")"
fi

printf 'Promotion preflight → overlay %q\n' "${overlay}"

# 1. Deterministic offline validation — always runs and needs no model. It checks
#    the eval-set structure and seed references, not candidate behavior.
printf '\n[1/3] Offline eval-set validation (eval:validate)...\n'
(cd evals && mise run eval:validate)

# 2. Optional model-backed behavior evidence. This is required before the script
#    will emit a promotion command.
if ((with_model)); then
	printf '\n[2/3] Model-backed evaluation (eval)...\n'
	# One task, one artifact. Groundedness and cost used to be separate campaigns; they
	# are flags and a drift warning on this run now, so there is nothing else to chain.
	(cd evals && mise run eval)
else
	printf '\n[2/3] Skipping model-backed behavior gate (pass --with-model to run it).\n'
fi

# 3. Prove the target overlay still renders before anyone promotes it.
printf '\n[3/3] Rendering the %q overlay...\n' "${overlay}"
kubectl kustomize "infra/k8s/overlays/${overlay}" >/dev/null
printf 'The %q overlay renders cleanly.\n' "${overlay}"

if ((! with_model)); then
	cat <<EOF

Offline preflight passed, but no promotion command was emitted because candidate
behavior was not evaluated. Re-run with --with-model when a model is configured.
EOF
	exit 0
fi

current_identity="$(tools/bin/source-identity --root . --mode release)"
current_commit="$(jq -er .revision <<<"${current_identity}")"
current_tree_digest="$(jq -er .tree_digest <<<"${current_identity}")"
[[ ${current_commit} == "${candidate_commit}" && ${current_tree_digest} == "${candidate_tree_digest}" ]] ||
	fail "promote: source changed during evaluation; rerun the gate on one clean commit"

printf '\nSource behavior gate passed for revision %s and tree %s.\n' \
	"${candidate_commit}" "${candidate_tree_digest}"
printf 'Build and deploy that unchanged source with:\n'
if [[ ${overlay} == gke ]]; then
	printf '  mise run gke:deploy\n'
else
	printf '  mise run platform:run\n'
fi
cat <<'EOF'

The command builds after this preflight; no image digest was evaluated here.
For production, scan and smoke the same immutable digest that you deploy, then
record it with the source tuple so rollback can target a known-good artifact.
EOF
