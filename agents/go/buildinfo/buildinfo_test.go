package buildinfo

import (
	"strings"
	"testing"
	"time"
)

const (
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	testDigest   = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testStamp    = "2026-08-09T10:11:12Z"
)

func TestParseReleaseBuildInformation(t *testing.T) {
	info, err := Parse(Raw{
		Mode: string(Release), Version: "v1.2.3", SourceIdentity: testRevision,
		Revision: testRevision, TreeDigest: testDigest, Timestamp: testStamp, Dirty: "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != Release || info.Version != "v1.2.3" || info.SourceIdentity != testRevision ||
		info.Revision != testRevision || info.TreeDigest != testDigest || info.Dirty ||
		!info.Timestamp.Equal(time.Date(2026, 8, 9, 10, 11, 12, 0, time.UTC)) {
		t.Fatalf("info = %#v", info)
	}
}

func TestParseDevelopmentBuildInformation(t *testing.T) {
	info, err := Parse(Raw{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != Development || info.Version != DevelopmentVersion ||
		info.SourceIdentity != DevelopmentIdentity || info.Revision != "" || !info.Dirty || !info.Timestamp.IsZero() {
		t.Fatalf("info = %#v", info)
	}

	dirtyDigest := "sha256:abcdefabcdef0123456789abcdef0123456789abcdef0123456789abcdef0123"
	info, err = Parse(Raw{
		Mode: string(Development), Version: DevelopmentVersion,
		SourceIdentity: "unknown+dirty.abcdefabcdef", TreeDigest: dirtyDigest,
		Timestamp: testStamp, Dirty: "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Dirty || info.Revision != "" || info.TreeDigest != dirtyDigest {
		t.Fatalf("info = %#v", info)
	}
}

func TestParseRejectsMalformedOrInconsistentReleaseInformation(t *testing.T) {
	valid := Raw{
		Mode: string(Release), Version: "v1.2.3", SourceIdentity: testRevision,
		Revision: testRevision, TreeDigest: testDigest, Timestamp: testStamp, Dirty: "false",
	}
	for name, mutate := range map[string]func(*Raw){
		"empty version":       func(raw *Raw) { raw.Version = "" },
		"templated version":   func(raw *Raw) { raw.Version = "{{.VERSION}}" },
		"malformed version":   func(raw *Raw) { raw.Version = "latest" },
		"leading-zero major":  func(raw *Raw) { raw.Version = "v01.2.3" },
		"leading-zero minor":  func(raw *Raw) { raw.Version = "v1.02.3" },
		"leading-zero patch":  func(raw *Raw) { raw.Version = "v1.2.03" },
		"empty revision":      func(raw *Raw) { raw.Revision = "" },
		"templated revision":  func(raw *Raw) { raw.Revision = "<no value>" },
		"mismatched identity": func(raw *Raw) { raw.SourceIdentity = strings.Repeat("a", 40) },
		"malformed digest":    func(raw *Raw) { raw.TreeDigest = "sha256:short" },
		"dirty release":       func(raw *Raw) { raw.Dirty = "true" },
		"missing timestamp":   func(raw *Raw) { raw.Timestamp = "" },
		"invalid timestamp":   func(raw *Raw) { raw.Timestamp = "yesterday" },
	} {
		t.Run(name, func(t *testing.T) {
			raw := valid
			mutate(&raw)
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%#v) succeeded", raw)
			}
		})
	}
}

func TestParseRejectsPartialOrInconsistentDevelopmentInformation(t *testing.T) {
	for name, raw := range map[string]Raw{
		"partial linker data": {Version: DevelopmentVersion},
		"release version": {
			Mode: string(Development), Version: "v1.2.3", SourceIdentity: testRevision,
			Revision: testRevision, TreeDigest: testDigest, Timestamp: testStamp, Dirty: "false",
		},
		"dirty with revision": {
			Mode: string(Development), Version: DevelopmentVersion,
			SourceIdentity: "unknown+dirty.0123456789ab", Revision: testRevision,
			TreeDigest: testDigest, Timestamp: testStamp, Dirty: "true",
		},
		"dirty marker mismatch": {
			Mode: string(Development), Version: DevelopmentVersion,
			SourceIdentity: "unknown+dirty.aaaaaaaaaaaa", TreeDigest: testDigest,
			Timestamp: testStamp, Dirty: "true",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(raw); err == nil {
				t.Fatalf("Parse(%#v) succeeded", raw)
			}
		})
	}
}

func TestValidateRejectsAHandBuiltInconsistentIdentity(t *testing.T) {
	t.Parallel()

	valid, err := Parse(Raw{
		Mode: string(Release), Version: "v1.2.3", SourceIdentity: testRevision,
		Revision: testRevision, TreeDigest: testDigest, Timestamp: testStamp, Dirty: "false",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("Validate(valid) error = %v", err)
	}

	valid.SourceIdentity = strings.Repeat("b", 40)
	if err := Validate(valid); err == nil {
		t.Fatal("Validate() accepted a release identity that disagrees with its revision")
	}
}
