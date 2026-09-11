package evals

import (
	"strings"
	"testing"
)

func TestSourceEvidenceRefusesDirtyTreesClaimingARevision(t *testing.T) {
	t.Parallel()
	clean := testSourceEvidence()
	if err := clean.Validate(); err != nil {
		t.Fatal(err)
	}
	dirty := SourceEvidence{TreeDigest: clean.TreeDigest, Dirty: true}
	if err := dirty.Validate(); err != nil {
		t.Fatal(err)
	}
	dirty.Revision = clean.Revision
	if err := dirty.Validate(); err == nil {
		t.Fatal("dirty checkout impersonated HEAD")
	}
	short := clean
	short.Revision = "abc"
	if err := short.Validate(); err == nil {
		t.Fatal("clean checkout accepted a partial revision")
	}
}

func testSourceEvidence() SourceEvidence {
	return SourceEvidence{
		Revision:   strings.Repeat("a", 40),
		TreeDigest: "sha256:" + strings.Repeat("b", 64),
	}
}
