package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckProbeInventoryRejectsAYmlWorkload pins the naming half of the probe
// inventory.
//
// The sweep reads `.yaml`, so the identical Deployment spelled `.yml` was
// rendered by kustomize and reviewed by nobody: it never reached the registry
// comparison, so it was neither missing nor unregistered — it was absent. The
// assertions below state both halves of that: the spelling is now a problem in
// its own right, and the inventory comparison still cannot see the workload,
// which is exactly why the name is what gets gated.
func TestCheckProbeInventoryRejectsAYmlWorkload(t *testing.T) {
	t.Parallel()
	root := copyManifests(t, probeManifests...)
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: shadow\n  namespace: agentops\nspec:\n  template:\n    spec:\n      containers:\n        - name: shadow\n"
	if err := os.WriteFile(filepath.Join(root, "infra", "k8s", "base", "shadow.yml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := problemMessages(checkProbeContracts(root, repositoryPages(t)))
	if !strings.Contains(messages, "Kubernetes manifests must be named .yaml") {
		t.Errorf("problems = %s, want the .yml workload rejected by name", messages)
	}
	if strings.Contains(messages, "unregistered: shadow") {
		t.Errorf("problems = %s, want the .yml workload to remain invisible to the inventory comparison", messages)
	}
}
