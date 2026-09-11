#!/usr/bin/env bash

# The same tiers after a rename the checker was not told about: base_managed_tools
# is now base_tools. This is the shape the real script drifted into, and the shape
# that used to leave three tiers comparing their prose against an empty set.

# --8<-- [start:doctor-tool-tiers]
readonly -a base_tools=(go jq)
readonly -a base_host_tools=(cc git)
readonly -a model_host_tools=(ollama)
readonly -a gateway_managed_tools=(yq)
readonly -a gateway_host_tools=(docker openssl)
readonly -a platform_tools=(k3d kubectl)
readonly -a gcp_platform_tools=(tofu)
readonly -a gcp_host_tools=(gcloud)
# --8<-- [end:doctor-tool-tiers]
