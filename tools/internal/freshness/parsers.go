package freshness

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	unstableTag = regexp.MustCompile(`(?i)(?:^|[._+-])(?:alpha|beta|rc|pre|preview|dev|nightly)\d*(?:$|[._+-])`)
	versionTag  = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)(?:[+-]k3s(\d+))?$`)
	helmChart   = regexp.MustCompile(`^\s*chart:\s+oci://(ghcr\.io/[^@\s]+)@(sha256:[0-9a-f]{64})\s*$`)
	helmVersion = regexp.MustCompile(`(?m)^# kagent-chart-version: (\d+\.\d+\.\d+)$`)
)

type version struct {
	major    int
	minor    int
	patch    int
	revision int
}

func (v version) less(other version) bool {
	left := [...]int{v.major, v.minor, v.patch, v.revision}
	right := [...]int{other.major, other.minor, other.patch, other.revision}
	for index := range left {
		if left[index] != right[index] {
			return left[index] < right[index]
		}
	}
	return false
}

func versionKey(tag string) (version, bool) {
	match := versionTag.FindStringSubmatch(tag)
	if match == nil {
		return version{}, false
	}
	values := make([]int, 4)
	for index := range values {
		if match[index+1] == "" {
			continue
		}
		parsed, err := strconv.Atoi(match[index+1])
		if err != nil {
			return version{}, false
		}
		values[index] = parsed
	}
	return version{major: values[0], minor: values[1], patch: values[2], revision: values[3]}, true
}

// LatestStableRelease selects the highest stable semantic release even when
// an upstream feed incorrectly labels an RC as non-prerelease.
func LatestStableRelease(document any) (StableRelease, bool) {
	items, ok := document.([]any)
	if !ok {
		return StableRelease{}, false
	}
	var selected StableRelease
	var selectedVersion version
	found := false
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["draft"] == true || item["prerelease"] == true {
			continue
		}
		tag, tagOK := item["tag_name"].(string)
		url, urlOK := item["html_url"].(string)
		candidateVersion, versionOK := versionKey(tag)
		if !tagOK || !urlOK || !versionOK || unstableTag.MatchString(tag) {
			continue
		}
		candidate := StableRelease{Tag: tag, URL: url}
		if assets, ok := item["assets"].([]any); ok {
			for _, rawAsset := range assets {
				asset, ok := rawAsset.(map[string]any)
				if !ok {
					continue
				}
				name, nameOK := asset["name"].(string)
				assetURL, assetURLOK := asset["browser_download_url"].(string)
				digest, digestOK := asset["digest"].(string)
				if nameOK && assetURLOK && digestOK {
					candidate.Assets = append(candidate.Assets, ReleaseAsset{Name: name, URL: assetURL, Digest: digest})
				}
			}
		}
		if !found || selectedVersion.less(candidateVersion) {
			selected, selectedVersion, found = candidate, candidateVersion, true
		}
	}
	return selected, found
}

// ParseMiseOutdated retains only actionable string versions.
func ParseMiseOutdated(document any) (map[string]MiseUpdate, error) {
	items, ok := document.(map[string]any)
	if !ok {
		return nil, errors.New("mise outdated output must be a JSON object")
	}
	updates := make(map[string]MiseUpdate)
	for name, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("mise outdated entries must be named JSON objects")
		}
		requested, requestedOK := item["requested"].(string)
		latest, latestOK := item["latest"].(string)
		if requestedOK && latestOK {
			updates[name] = MiseUpdate{Requested: requested, Latest: latest}
		}
	}
	return updates, nil
}

func parseMiseOutdatedJSON(data []byte) (map[string]MiseUpdate, error) {
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, err
	}
	return ParseMiseOutdated(document)
}

// MiseResult returns the displayed latest version and triage status.
//
// Absence is an answer, but only because miseOutdatedArgs carries --bump: mise
// then lists every tool that has a newer version, so a pin it did not list and
// did not warn about is a pin nothing is newer than. Without that flag mise
// honors an exact request as a ceiling and lists nothing at all, which is what
// made this table a guaranteed all-clear — 30 unasked questions rendering as 30
// upstream confirmations. A row that arrives with no version is a gap rather
// than an answer: runMiseOutdated records a tool mise warned it could not
// resolve that way, because mise omits that row and still exits 0, and reading
// that omission as "nothing newer" would restore the same rubber stamp.
func MiseResult(pinned string, update *MiseUpdate, available bool) (string, string) {
	if !available {
		return "unavailable", "UNAVAILABLE"
	}
	if update == nil {
		return pinned, "CURRENT"
	}
	if update.Latest == "" {
		return "unchecked", "UNKNOWN"
	}
	if update.Latest == pinned {
		return update.Latest, "CURRENT"
	}
	return update.Latest, "REVIEW"
}

// ParseHelmCharts extracts the reviewed version and immutable chart sources.
func ParseHelmCharts(text string) (string, []HelmChart) {
	version := ""
	if match := helmVersion.FindStringSubmatch(text); match != nil {
		version = match[1]
	}
	releaseName := ""
	namePattern := regexp.MustCompile(`^\s*-\s+name:\s+([a-z0-9-]+)\s*$`)
	var charts []HelmChart
	for _, line := range strings.Split(text, "\n") {
		if match := namePattern.FindStringSubmatch(line); match != nil {
			releaseName = match[1]
			continue
		}
		if match := helmChart.FindStringSubmatch(line); match != nil {
			name := releaseName
			if name == "" {
				name = "unnamed"
			}
			charts = append(charts, HelmChart{Name: name, Source: match[1], Digest: match[2]})
		}
	}
	return version, charts
}

// OllamaAssetFromWorkflow binds one archive URL to its checked SHA-256.
func OllamaAssetFromWorkflow(text string) (OllamaAssetPin, bool) {
	urlPattern := regexp.MustCompile(`"(https://github\.com/ollama/ollama/releases/download/(v\d+\.\d+\.\d+)/(ollama-[^"]+))"`)
	digestPattern := regexp.MustCompile(`echo\s+"([0-9a-f]{64})\s+\$\{archive\}"`)
	urlMatch := urlPattern.FindStringSubmatch(text)
	digestMatch := digestPattern.FindStringSubmatch(text)
	if urlMatch == nil || digestMatch == nil {
		return OllamaAssetPin{}, false
	}
	return OllamaAssetPin{Tag: urlMatch[2], Name: urlMatch[3], URL: urlMatch[1], Digest: "sha256:" + digestMatch[1]}, true
}

func normalizedReleaseTag(component, tag string) string {
	if component == "k3s" {
		return strings.ReplaceAll(tag, "-k3s", "+k3s")
	}
	return tag
}

// ReleaseResult compares a repository pin with an upstream stable tag.
func ReleaseResult(component, pinned, latest string) string {
	if normalizedReleaseTag(component, pinned) == normalizedReleaseTag(component, latest) {
		return "CURRENT"
	}
	return "REVIEW"
}

func majorMinor(value string) (int, int, bool) {
	match := regexp.MustCompile(`^v?(\d+)\.(\d+)`).FindStringSubmatch(value)
	if match == nil {
		return 0, 0, false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	return major, minor, true
}

func kubernetesSkewResult(k3s, kubectl string) (string, string) {
	serverMajor, serverMinor, serverOK := majorMinor(k3s)
	clientMajor, clientMinor, clientOK := majorMinor(kubectl)
	if !serverOK || !clientOK || serverMajor != clientMajor {
		return "unknown", "REVIEW"
	}
	distance := clientMinor - serverMinor
	status := "REVIEW"
	if distance >= -1 && distance <= 1 {
		status = "CURRENT"
	}
	return fmt.Sprintf("%+d minor", distance), status
}

// ParseImageReference applies Docker's default registry and library rules.
func ParseImageReference(reference string) (ImageReference, bool) {
	name, digest, ok := strings.Cut(reference, "@")
	if !ok || !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		return ImageReference{}, false
	}
	first, remainder, slash := strings.Cut(name, "/")
	registry := "docker.io"
	repositoryAndTag := name
	if slash && (strings.ContainsAny(first, ".:") || first == "localhost") {
		registry, repositoryAndTag = first, remainder
	}
	repository, tag := repositoryAndTag, ""
	last := repositoryAndTag[strings.LastIndex(repositoryAndTag, "/")+1:]
	if strings.Contains(last, ":") {
		repository, tag, _ = strings.Cut(repositoryAndTag, ":")
		// Cut uses the first colon; registry was already removed, and image
		// repository names themselves cannot contain a colon.
	}
	if registry == "docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return ImageReference{Registry: registry, Repository: repository, Tag: tag, Digest: digest}, true
}
