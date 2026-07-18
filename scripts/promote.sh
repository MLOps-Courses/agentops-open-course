#!/usr/bin/env bash

# Eval-gated promotion (Chapter 6.7): refuse to roll out a new agent image or
# prompt version unless the evaluation gate passes first. Evaluation is the
# promotion criterion here, not an afterthought run once the change already
# shipped. The deterministic offline gate always runs; model-backed evidence is
# opt-in with --with-model. Nothing is applied to a cluster — the script gates
# and then prints the exact promote and rollback commands for you to run.
#
#   scripts/promote.sh                 # gate + render the local overlay
#   scripts/promote.sh gke             # gate + render the gke overlay
#   scripts/promote.sh --with-model    # also run the model-backed evals

set -Eeuo pipefail

overlay=local
with_model=0
for arg in "$@"; do
	case ${arg} in
	--with-model) with_model=1 ;;
	--*)
		printf 'promote: unknown flag %q\n' "${arg}" >&2
		exit 2
		;;
	*) overlay=${arg} ;;
	esac
done

printf 'Eval-gated promotion → overlay %q\n' "${overlay}"

# 1. Deterministic offline gate — always runs, needs no model. A failure here
#    means the change regressed a behavior the committed eval set pins, so it
#    must not promote.
printf '\n[1/3] Offline evaluation gate (eval:validate)...\n'
(cd agents/python && mise run eval:validate)

# 2. Optional model-backed evidence: trajectory, groundedness, and cost. Skipped
#    by default so the gate stays runnable without a model or a cluster.
if ((with_model)); then
	printf '\n[2/3] Model-backed evals (eval:mlflow, eval:ground)...\n'
	(cd agents/python && mise run eval:mlflow && mise run eval:ground)
else
	printf '\n[2/3] Skipping model-backed evals (pass --with-model to include them).\n'
fi

# 3. Prove the target overlay still renders before anyone promotes it.
printf '\n[3/3] Rendering the %q overlay...\n' "${overlay}"
kustomize build "infra/k8s/overlays/${overlay}" >/dev/null
printf 'The %q overlay renders cleanly.\n' "${overlay}"

cat <<EOF

Eval gate passed. Promote the new image with:
  cd infra && SKAFFOLD_DEFAULT_REPO=registry.localhost:5050 skaffold run --filename skaffold.yaml --profile ${overlay}

Roll a prompt regression back instantly, without a redeploy, by pinning the
previous registry version (Chapter 4.4):
  AGENT_PROMPT_URI=prompts:/agentops-agent-instruction/<previous-version>
EOF
