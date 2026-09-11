package conventions

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repositoryRoot locates the checkout. The manifest registries name real
// repository paths, so every fixture has to recreate those exact paths.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// copyManifests mirrors reviewed manifest directories into a scratch root, so a
// drift test can mutate one file instead of duplicating the tree it asserts on.
func copyManifests(t *testing.T, directories ...string) string {
	t.Helper()
	source := repositoryRoot(t)
	target := t.TempDir()
	for _, directory := range directories {
		base := filepath.Join(source, filepath.FromSlash(directory))
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			name, relErr := filepath.Rel(source, path)
			if relErr != nil {
				return relErr
			}
			destination := filepath.Join(target, name)
			if entry.IsDir() {
				return os.MkdirAll(destination, 0o750)
			}
			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			return os.WriteFile(destination, content, 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return target
}

// mutateFile rewrites one copied manifest and fails when its anchor has itself
// drifted, so a stale test can never pass by editing nothing.
func mutateFile(t *testing.T, root, where, anchor, replacement string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(where))
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(content), anchor, replacement, 1)
	if updated == string(content) {
		t.Fatalf("%s no longer contains %q", where, anchor)
	}
	if err := os.WriteFile(path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
}

func repositoryPages(t *testing.T) pageSet {
	t.Helper()
	pages, err := loadPages(repositoryRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return pages
}

// mutatePage returns a copy of the page set with one published claim rewritten.
func mutatePage(t *testing.T, pages pageSet, where, anchor, replacement string) pageSet {
	t.Helper()
	updated := strings.Replace(pages[where], anchor, replacement, 1)
	if updated == pages[where] {
		t.Fatalf("%s no longer contains %q", where, anchor)
	}
	mutated := make(pageSet, len(pages))
	for path, text := range pages {
		mutated[path] = text
	}
	mutated[where] = updated
	return mutated
}

func TestDecodeManifestsReadsEveryDocumentInTheStream(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	where := "infra/example.yaml"
	path := filepath.Join(root, filepath.FromSlash(where))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	stream := "kind: Deployment\nmetadata:\n  name: first\n---\nkind: Service\nmetadata:\n  name: second\n---\nkind: NetworkPolicy\nmetadata:\n  name: third\n"
	if err := os.WriteFile(path, []byte(stream), 0o600); err != nil {
		t.Fatal(err)
	}
	documents, err := decodeManifests[manifestWorkload](root, where)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 3 {
		t.Fatalf("documents = %d, want every document in the stream", len(documents))
	}
	if documents[2].Metadata.Name != "third" {
		t.Fatalf("last document = %q, want the third resource", documents[2].Metadata.Name)
	}
	if _, err := decodeManifests[manifestWorkload](root, "infra/missing.yaml"); err == nil {
		t.Fatal("expected an error for a missing manifest")
	}
}
