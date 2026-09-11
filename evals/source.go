package evals

import (
	"errors"
	"regexp"
)

var (
	sourceRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	treeDigestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// SourceEvidence says which code a run evaluated: the commit it came from and
// whether the working tree still matched that commit.
//
// TreeDigest is deliberately not serialized. A reader of the artifact can act
// on a revision and a dirty flag; the digest exists only so the harness can
// compare this checkout with the agent binary's own `version` tuple, and a
// dirty checkout has no revision to compare instead.
type SourceEvidence struct {
	Revision   string `json:"revision"`
	TreeDigest string `json:"-"`
	Dirty      bool   `json:"dirty"`
}

func (s SourceEvidence) Validate() error {
	if s.Dirty {
		// A dirty tree is not the commit it sits on, so it never claims one.
		if s.Revision != "" {
			return errors.New("a dirty checkout must not claim a revision")
		}
		return nil
	}
	if !sourceRevisionPattern.MatchString(s.Revision) {
		return errors.New("a clean checkout must name one full lowercase revision")
	}
	return nil
}
