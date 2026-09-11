#!/usr/bin/env bash

# A trimmed stand-in for scripts/doctor.sh: only the tier arrays the maintainer
# drift check reads, with a short invented tool list so the fixture never has to
# move when the real toolchain does.

# --8<-- [start:doctor-tool-tiers]
readonly -a base_managed_tools=(go jq)
readonly -a base_host_tools=(cc git)
readonly -a model_host_tools=(ollama)
readonly -a gateway_managed_tools=(yq)
readonly -a gateway_host_tools=(docker openssl)
readonly -a platform_tools=(k3d kubectl)
readonly -a gcp_platform_tools=(tofu)
readonly -a gcp_host_tools=(gcloud)
# --8<-- [end:doctor-tool-tiers]
