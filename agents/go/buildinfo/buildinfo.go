// Package buildinfo is the single authority for the binary's build identity.
package buildinfo

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	Development Mode = "development"
	Release     Mode = "release"

	DevelopmentVersion  = "development"
	DevelopmentIdentity = "unknown+development"
)

var (
	version        string
	sourceIdentity string
	revision       string
	treeDigest     string
	buildTimestamp string
	buildMode      string
	dirty          string
)

var (
	versionPattern  = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type Raw struct {
	Mode           string
	Version        string
	SourceIdentity string
	Revision       string
	TreeDigest     string
	Timestamp      string
	Dirty          string
}

type Info struct {
	Timestamp      time.Time `json:"build_timestamp,omitzero"`
	Mode           Mode      `json:"mode"`
	Version        string    `json:"version"`
	SourceIdentity string    `json:"source_identity"`
	Revision       string    `json:"revision,omitempty"`
	TreeDigest     string    `json:"tree_digest,omitempty"`
	Dirty          bool      `json:"dirty"`
}

func Current() (Info, error) {
	return Parse(Raw{
		Mode: buildMode, Version: version, SourceIdentity: sourceIdentity,
		Revision: revision, TreeDigest: treeDigest, Timestamp: buildTimestamp, Dirty: dirty,
	})
}

// Validate proves that an Info value could have been produced by Parse.
//
// Info is exported so callers can persist the typed tuple without depending on
// linker variables. That also means a struct literal can bypass Parse; every
// durable or public boundary calls Validate before trusting one.
func Validate(info Info) error {
	if info == (Info{
		Mode: Development, Version: DevelopmentVersion,
		SourceIdentity: DevelopmentIdentity, Dirty: true,
	}) {
		return nil
	}
	timestamp := ""
	if !info.Timestamp.IsZero() {
		timestamp = info.Timestamp.UTC().Format(time.RFC3339)
	}
	_, err := Parse(Raw{
		Mode:           string(info.Mode),
		Version:        info.Version,
		SourceIdentity: info.SourceIdentity,
		Revision:       info.Revision,
		TreeDigest:     info.TreeDigest,
		Timestamp:      timestamp,
		Dirty:          strconv.FormatBool(info.Dirty),
	})
	return err
}

func Parse(raw Raw) (Info, error) {
	if raw == (Raw{}) {
		// `go run` has no linker contract. Keep that state explicit and
		// conservative instead of manufacturing a release revision.
		return Info{
			Mode: Development, Version: DevelopmentVersion,
			SourceIdentity: DevelopmentIdentity, Dirty: true,
		}, nil
	}
	if strings.TrimSpace(raw.Mode) != raw.Mode || strings.TrimSpace(raw.Version) != raw.Version ||
		strings.TrimSpace(raw.SourceIdentity) != raw.SourceIdentity || strings.TrimSpace(raw.Revision) != raw.Revision ||
		strings.TrimSpace(raw.TreeDigest) != raw.TreeDigest || strings.TrimSpace(raw.Timestamp) != raw.Timestamp ||
		strings.TrimSpace(raw.Dirty) != raw.Dirty {
		return Info{}, errors.New("build information fields must be trimmed")
	}
	mode := Mode(raw.Mode)
	if mode != Development && mode != Release {
		return Info{}, fmt.Errorf("build mode %q is invalid", raw.Mode)
	}
	dirtyValue, err := strconv.ParseBool(raw.Dirty)
	if err != nil || (raw.Dirty != "true" && raw.Dirty != "false") {
		return Info{}, fmt.Errorf("build dirty state %q must be true or false", raw.Dirty)
	}
	if !digestPattern.MatchString(raw.TreeDigest) {
		return Info{}, fmt.Errorf("build tree digest %q must be a sha256 digest", raw.TreeDigest)
	}
	timestamp, err := time.Parse(time.RFC3339, raw.Timestamp)
	if err != nil {
		return Info{}, fmt.Errorf("build timestamp %q must use RFC3339: %w", raw.Timestamp, err)
	}
	info := Info{
		Mode: mode, Version: raw.Version, SourceIdentity: raw.SourceIdentity,
		Revision: raw.Revision, TreeDigest: raw.TreeDigest, Timestamp: timestamp, Dirty: dirtyValue,
	}
	if mode == Release {
		if !versionPattern.MatchString(raw.Version) {
			return Info{}, fmt.Errorf("release version %q must be a stable semantic version", raw.Version)
		}
		if dirtyValue {
			return Info{}, errors.New("a release build cannot be dirty")
		}
		if !revisionPattern.MatchString(raw.Revision) {
			return Info{}, fmt.Errorf("release revision %q must be a full lowercase commit SHA", raw.Revision)
		}
		if raw.SourceIdentity != raw.Revision {
			return Info{}, errors.New("release source identity must equal its revision")
		}
		return info, nil
	}

	if raw.Version != DevelopmentVersion {
		return Info{}, fmt.Errorf("development version must be %q", DevelopmentVersion)
	}
	if dirtyValue {
		if raw.Revision != "" {
			return Info{}, errors.New("dirty development source cannot claim a revision")
		}
		want := "unknown+dirty." + strings.TrimPrefix(raw.TreeDigest, "sha256:")[:12]
		if raw.SourceIdentity != want {
			return Info{}, fmt.Errorf("dirty development source identity %q must equal %q", raw.SourceIdentity, want)
		}
		return info, nil
	}
	if !revisionPattern.MatchString(raw.Revision) || raw.SourceIdentity != raw.Revision {
		return Info{}, errors.New("clean development source identity must equal a full lowercase revision")
	}
	return info, nil
}
