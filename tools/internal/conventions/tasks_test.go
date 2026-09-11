package conventions

import (
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// nativeToolBuild is the task that compiles tools/bin/, and nativeToolPrefix is
// the path a task shells out to once it has.
const (
	nativeToolBuild  = "tools:built"
	nativeToolPrefix = "tools/bin/"
)

// tasksMissingTheNativeToolBuild returns every task in a mise manifest that runs
// a binary from tools/bin/ without declaring the task that produces it.
//
// The producing task is exempt from its own rule: it is what the others depend
// on, and a task cannot depend on itself.
func tasksMissingTheNativeToolBuild(manifest string) []string {
	var missing []string
	for name := range taskNames(manifest) {
		if name == nativeToolBuild {
			continue
		}
		for _, command := range taskCommands(manifest, name) {
			if !strings.Contains(command, nativeToolPrefix) {
				continue
			}
			if !slices.Contains(taskDependencies(manifest, name), nativeToolBuild) {
				missing = append(missing, name)
			}
			break
		}
	}
	// Map iteration is randomized, so an unsorted failure message would name the
	// same tasks in a different order on every run.
	sort.Strings(missing)
	return missing
}

// TestEveryNativeToolTaskDependsOnTheBuildThatProducesIt gates by hand what was
// previously audited by hand.
//
// tools/bin/ is gitignored and run_auto_install is off, so a task that shells out
// to one of those binaries without declaring tools:built either fails with a bare
// "command not found" on a fresh checkout, or — worse — succeeds against whichever
// binary was compiled last, validating the tree against rules nobody is running.
// Every task in the manifest declares the dependency today; this keeps the next
// one from silently missing it.
func TestEveryNativeToolTaskDependsOnTheBuildThatProducesIt(t *testing.T) {
	t.Run("the repository manifest", func(t *testing.T) {
		manifest, err := filepath.Abs(filepath.Join("..", "..", "..", "mise.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if missing := tasksMissingTheNativeToolBuild(manifest); len(missing) != 0 {
			t.Errorf("tasks running %s without depends = [%q]: %v",
				nativeToolPrefix, nativeToolBuild, missing)
		}
	})

	// The fixtures below prove the rule can fail, in both shapes `run` takes: a
	// bare string and an array of commands. Without them a broken collector would
	// report the manifest clean and this test would pass forever.
	for _, test := range []struct {
		name     string
		manifest string
		want     []string
	}{
		{
			name: "a string run without the dependency",
			manifest: `[tasks."check:skills"]
run = "tools/bin/conventions skills"
`,
			want: []string{"check:skills"},
		},
		{
			name: "an array run whose second command is missed",
			manifest: `[tasks."check:docs"]
depends = ["check:data"]
run = ["hugo build", "tools/bin/conventions docs"]
`,
			want: []string{"check:docs"},
		},
		{
			name: "the dependency declared alongside others",
			manifest: `[tasks."eval:offline"]
depends = ["tools:built", "agent:built"]
run = "tools/bin/fake-model --port 11435"
`,
		},
		{
			name: "a task that shells out to nothing native",
			manifest: `[tasks."check:links"]
run = "lychee --offline README.md"
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := filepath.Join(t.TempDir(), "mise.toml")
			if err := os.WriteFile(manifest, []byte(test.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := tasksMissingTheNativeToolBuild(manifest); !slices.Equal(got, test.want) {
				t.Errorf("tasksMissingTheNativeToolBuild() = %v, want %v", got, test.want)
			}
		})
	}
}
