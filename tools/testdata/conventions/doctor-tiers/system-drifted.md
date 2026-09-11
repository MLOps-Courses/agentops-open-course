The same page after `jq` joined the base tier and this sentence was not updated.

{{< include path="scripts/doctor.sh" region="doctor-tool-tiers" lang="bash" >}}

The base doctor checks mise-managed `go`, plus the host prerequisites `cc` and `git`. Each heavier profile adds its own tier on top:

- **model** adds `ollama` and the configured local model.
- **gateway** adds host `docker` and `openssl` plus mise-managed `yq`.
- **platform** checks `k3d` and `kubectl`, plus the gateway tier.
- **gcp** checks `gcloud` and `tofu`, plus the gateway tier.
