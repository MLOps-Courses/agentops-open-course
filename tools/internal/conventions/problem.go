package conventions

import (
	"fmt"
	"path/filepath"
)

// Problem is one actionable convention failure. Where is always repository-
// relative for source checks and site-relative for rendered checks.
type Problem struct {
	Where   string
	Message string
}

func (p Problem) String() string { return fmt.Sprintf("%s: %s", p.Where, p.Message) }

func relative(root, path string) string {
	value, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(value)
}

func problem(where, format string, arguments ...any) Problem {
	return Problem{Where: filepath.ToSlash(where), Message: fmt.Sprintf(format, arguments...)}
}
