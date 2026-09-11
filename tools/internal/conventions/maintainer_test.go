package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorTiersFixture holds trimmed doctor scripts and the profile prose graded
// against them. They live in testdata rather than under scripts/, so the repository's
// own shell linters never collect a deliberately incomplete doctor and the docs gate
// never collects a deliberately drifted page; the tests plant them where the check
// looks.
const doctorTiersFixture = "../../testdata/conventions/doctor-tiers"

// plantDoctorTiers builds a root holding one fixture doctor script, and the page set
// whose System page the tier comparison reads. Every other surface checkMaintainerDrift
// reads is absent on purpose: those problems are noise here, and asserting on messages
// rather than on the count keeps this test about the tiers.
func plantDoctorTiers(t *testing.T, script, page string) (string, pageSet) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o750); err != nil {
		t.Fatal(err)
	}
	content, readErr := os.ReadFile(filepath.Join(doctorTiersFixture, script))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if writeErr := os.WriteFile(filepath.Join(root, "scripts", "doctor.sh"), content, 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	system, systemErr := os.ReadFile(filepath.Join(doctorTiersFixture, page))
	if systemErr != nil {
		t.Fatal(systemErr)
	}
	return root, pageSet{"content/1. Setup/1.0. System.md": string(system)}
}

// A tier whose prose omits a tool its array declares must be rejected. The base,
// model, and gateway bullets could not fail this way while the check read array names
// the script had renamed away.
func TestDoctorTierDriftIsRejected(t *testing.T) {
	root, pages := plantDoctorTiers(t, "doctor.sh", "system-drifted.md")

	messages := problemMessages(checkMaintainerDrift(root, pages))
	if !strings.Contains(messages, "doctor base tool list drifted") || !strings.Contains(messages, "jq") {
		t.Errorf("base tier prose missing a declared tool was not seen: %s", messages)
	}

	root, pages = plantDoctorTiers(t, "doctor.sh", "system.md")
	messages = problemMessages(checkMaintainerDrift(root, pages))
	if strings.Contains(messages, "tool list drifted") {
		t.Errorf("prose that matches every tier was reported as drifted: %s", messages)
	}
}

// The defect this test pins: a checker naming an array the script no longer declares
// used to compare against a nil set, which passes whatever the page says. The rename
// must be reported from both sides — the tier that lost its array, and the array that
// belongs to no tier.
func TestDoctorTierNamingAMissingArrayIsReported(t *testing.T) {
	root, pages := plantDoctorTiers(t, "doctor-renamed.sh", "system.md")

	messages := problemMessages(checkMaintainerDrift(root, pages))
	if !strings.Contains(messages, "doctor base tier reads base_managed_tools, which the script no longer declares") {
		t.Errorf("renamed tier array was not reported: %s", messages)
	}
	if !strings.Contains(messages, "doctor array base_tools belongs to no documented tier") {
		t.Errorf("array no tier consumes was not reported: %s", messages)
	}
}
