package conventions

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeManifests reads one repository manifest and returns every YAML document
// it holds. Kubernetes manifests are `---` separated streams, so the single
// yaml.Unmarshal this package uses for one-document workflow files would see
// only the first resource and happily pass a file whose remaining documents had
// been gutted.
func decodeManifests[Document any](root, where string) ([]Document, error) {
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(where)))
	if err != nil {
		return nil, err
	}
	var documents []Document
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var document Document
		decodeErr := decoder.Decode(&document)
		if errors.Is(decodeErr, io.EOF) {
			return documents, nil
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		documents = append(documents, document)
	}
}

// sortedRegistryKeys keeps registry iteration deterministic. portContracts
// ranges its map directly, so its problem order changes between runs; a
// manifest registry reports several problems per entry, so stable order is what
// makes two failing runs comparable.
func sortedRegistryKeys[Value any](values map[string]Value) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// formatSet mirrors formatPorts for string inventories: an empty difference has
// to read as "none" instead of leaving a blank hole in the message.
func formatSet(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

// labelKey renders a label selector as a stable, sorted string so a registry
// entry and a decoded manifest reduce to the same comparable key.
func labelKey(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for name, value := range labels {
		pairs = append(pairs, name+"="+value)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, ",")
}
