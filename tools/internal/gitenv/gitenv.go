// Package gitenv removes an inherited Git repository from a child process.
//
// Git exports GIT_DIR, GIT_INDEX_FILE and their relatives to every hook it runs,
// and those variables outrank `git -C`: a command that names a directory still
// reads and writes the exporting repository. Anything in this module that runs
// `git` against a directory it was given — the source-identity resolver, and the
// tests that build throwaway repositories — has to strip them first, or it
// silently answers for the wrong tree. `mise run test` is a lefthook pre-push
// command, so this is the ordinary case rather than an exotic one.
package gitenv

import (
	"os"
	"os/exec"
	"strings"
)

// inherited names the variables that redirect git away from the directory a
// caller named. GIT_CONFIG* are included because a hook-supplied configuration
// can redefine `core.worktree` and reach the same result by another route.
var inherited = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_PREFIX",
	"GIT_CONFIG",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
}

// Detach points cmd at the directory it names rather than at any repository its
// environment carries. It leaves every other variable alone, because git still
// needs PATH, HOME and the terminal settings it was started with.
func Detach(cmd *exec.Cmd) *exec.Cmd {
	environment := cmd.Env
	if environment == nil {
		environment = os.Environ()
	}
	kept := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if !isInherited(name) {
			kept = append(kept, entry)
		}
	}
	cmd.Env = kept
	return cmd
}

func isInherited(name string) bool {
	for _, candidate := range inherited {
		if name == candidate {
			return true
		}
	}
	return false
}
