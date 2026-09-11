package conventions

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	stableVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	datePattern          = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	releaseHeading       = regexp.MustCompile(`^## \[[0-9]+\.[0-9]+\.[0-9]+\] - `)
	repositoryPattern    = regexp.MustCompile(`^[^/]+/[^/]+$`)
)

type ReleaseMetadata struct {
	Version    string
	Date       string
	Repository string
}

func ValidateReleaseMetadata(root, expectedTag, expectedRepository string) (ReleaseMetadata, error) {
	versionContent, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return ReleaseMetadata{}, fmt.Errorf("read VERSION: %w", err)
	}
	version := strings.TrimSpace(string(versionContent))
	if !stableVersionPattern.MatchString(version) {
		return ReleaseMetadata{}, fmt.Errorf("VERSION is not stable SemVer: %q", version)
	}
	citation, err := citationValues(filepath.Join(root, "CITATION.cff"))
	if err != nil {
		return ReleaseMetadata{}, err
	}
	if citation["version"] != version {
		return ReleaseMetadata{}, fmt.Errorf("VERSION %s and CITATION.cff version %s disagree", version, citation["version"])
	}
	date := citation["date-released"]
	if !datePattern.MatchString(date) {
		return ReleaseMetadata{}, fmt.Errorf("CITATION.cff date-released is not YYYY-MM-DD: %q", date)
	}
	repositoryURL := strings.TrimSuffix(citation["repository-code"], "/")
	repository := strings.TrimPrefix(repositoryURL, "https://github.com/")
	if !strings.HasPrefix(repositoryURL, "https://github.com/") || !repositoryPattern.MatchString(repository) {
		return ReleaseMetadata{}, errors.New("CITATION.cff repository-code must identify one GitHub owner/repository")
	}
	if expectedRepository != "" && !repositoryPattern.MatchString(expectedRepository) {
		return ReleaseMetadata{}, fmt.Errorf("expected repository is not an owner/repository: %q", expectedRepository)
	}
	if expectedRepository != "" && repository != expectedRepository {
		return ReleaseMetadata{}, fmt.Errorf("CITATION.cff repository %s does not match release repository %s", repository, expectedRepository)
	}
	changelogContent, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		return ReleaseMetadata{}, fmt.Errorf("read CHANGELOG.md: %w", err)
	}
	lines := strings.Split(string(changelogContent), "\n")
	newest := ""
	for _, line := range lines {
		if releaseHeading.MatchString(line) {
			newest = line
			break
		}
	}
	expectedHeading := fmt.Sprintf("## [%s] - %s", version, date)
	if newest != expectedHeading {
		return ReleaseMetadata{}, fmt.Errorf("newest changelog heading must be %q, got %q", expectedHeading, newest)
	}
	expectedLink := fmt.Sprintf("[%s]: %s/releases/tag/v%s", version, repositoryURL, version)
	if !slicesContains(lines, expectedLink) {
		return ReleaseMetadata{}, fmt.Errorf("CHANGELOG.md is missing %s", expectedLink)
	}
	if expectedTag != "" && expectedTag != "v"+version {
		return ReleaseMetadata{}, fmt.Errorf("tag %s does not match source version v%s", expectedTag, version)
	}
	return ReleaseMetadata{
		Version: version, Date: date,
		Repository: repository,
	}, nil
}

func citationValues(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open CITATION.cff: %w", err)
	}
	defer func() { _ = file.Close() }()
	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, " ") || !strings.Contains(line, ":") {
			continue
		}
		key, value, _ := strings.Cut(line, ":")
		values[key] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read CITATION.cff: %w", err)
	}
	for _, required := range []string{"version", "date-released", "repository-code"} {
		if values[required] == "" {
			return nil, fmt.Errorf("CITATION.cff is missing %s", required)
		}
	}
	return values, nil
}

func slicesContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
