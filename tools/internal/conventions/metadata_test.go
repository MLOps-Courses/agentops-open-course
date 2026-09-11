package conventions

import (
	"os"
	"path/filepath"
	"testing"
)

func writeMetadataFixture(t *testing.T, root, path, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func validMetadataFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeMetadataFixture(t, root, "VERSION", "1.2.3\n")
	writeMetadataFixture(t, root, "CITATION.cff", `cff-version: 1.2.0
version: 1.2.3
date-released: "2026-08-08"
repository-code: https://github.com/example/course
`)
	writeMetadataFixture(t, root, "CHANGELOG.md", `# Changelog

## [1.2.3] - 2026-08-08

[1.2.3]: https://github.com/example/course/releases/tag/v1.2.3
`)
	return root
}

func TestValidateReleaseMetadataAcceptsConsistentFiles(t *testing.T) {
	metadata, err := ValidateReleaseMetadata(validMetadataFixture(t), "v1.2.3", "example/course")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != "1.2.3" || metadata.Date != "2026-08-08" || metadata.Repository != "example/course" {
		t.Fatalf("metadata = %#v", metadata)
	}
}

func TestValidateReleaseMetadataRejectsDrift(t *testing.T) {
	root := validMetadataFixture(t)
	writeMetadataFixture(t, root, "VERSION", "1.2.4\n")
	if _, err := ValidateReleaseMetadata(root, "", ""); err == nil {
		t.Fatal("ValidateReleaseMetadata error = nil")
	}
}

func TestValidateReleaseMetadataRejectsLeadingZeroVersion(t *testing.T) {
	root := validMetadataFixture(t)
	writeMetadataFixture(t, root, "VERSION", "01.2.3\n")
	writeMetadataFixture(t, root, "CITATION.cff", `cff-version: 1.2.0
version: 01.2.3
date-released: "2026-08-08"
repository-code: https://github.com/example/course
`)
	writeMetadataFixture(t, root, "CHANGELOG.md", `# Changelog

## [01.2.3] - 2026-08-08

[01.2.3]: https://github.com/example/course/releases/tag/v01.2.3
`)
	if _, err := ValidateReleaseMetadata(root, "", ""); err == nil {
		t.Fatal("ValidateReleaseMetadata accepted invalid SemVer leading zeros")
	}
}

func TestValidateReleaseMetadataRejectsWrongTag(t *testing.T) {
	if _, err := ValidateReleaseMetadata(validMetadataFixture(t), "v2.0.0", ""); err == nil {
		t.Fatal("ValidateReleaseMetadata error = nil")
	}
}

func TestValidateReleaseMetadataRejectsWrongRepository(t *testing.T) {
	if _, err := ValidateReleaseMetadata(validMetadataFixture(t), "v1.2.3", "example/new-course"); err == nil {
		t.Fatal("ValidateReleaseMetadata error = nil")
	}
}
