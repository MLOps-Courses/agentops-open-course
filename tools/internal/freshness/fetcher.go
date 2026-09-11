package freshness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const httpTimeout = 10 * time.Second

const registryAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// Fetcher is the read-only network seam used by the report. Tests provide a
// deterministic fake; production uses HTTPFetcher with a bounded client.
type Fetcher interface {
	Get(context.Context, string, http.Header) ([]byte, http.Header, error)
	JSON(context.Context, string, http.Header) (any, error)
}

// HTTPFetcher reads HTTPS endpoints and optionally authenticates GitHub API
// requests. It has no method capable of issuing a write request.
type HTTPFetcher struct {
	client *http.Client
	token  string
}

// NewHTTPFetcher constructs the production read-only client.
func NewHTTPFetcher(githubToken string) *HTTPFetcher {
	return &HTTPFetcher{client: &http.Client{Timeout: httpTimeout}, token: githubToken}
}

// HTTPStatusError preserves the response headers needed for registry bearer
// challenges while keeping a bounded diagnostic.
type HTTPStatusError struct {
	Headers    http.Header
	URL        string
	StatusCode int
}

func (e *HTTPStatusError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.URL, e.StatusCode) }

func (f *HTTPFetcher) Get(ctx context.Context, endpoint string, headers http.Header) ([]byte, http.Header, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, nil, fmt.Errorf("freshness source must use HTTPS: %q", endpoint)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "agentops-open-course-freshness")
	for key, values := range headers {
		request.Header.Del(key)
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	if f.token != "" && parsed.Host == "api.github.com" {
		request.Header.Set("Authorization", "Bearer "+f.token)
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	response, err := f.client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %s", endpoint, cleanError(err))
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, response.Header.Clone(), &HTTPStatusError{StatusCode: response.StatusCode, Headers: response.Header.Clone(), URL: endpoint}
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %s", endpoint, cleanError(err))
	}
	return body, response.Header.Clone(), nil
}

func (f *HTTPFetcher) JSON(ctx context.Context, endpoint string, headers http.Header) (any, error) {
	body, _, err := f.Get(ctx, endpoint, headers)
	if err != nil {
		return nil, err
	}
	var document any
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON: %s", endpoint, cleanError(err))
	}
	return document, nil
}

func cleanError(err any) string {
	value := strings.Join(strings.Fields(fmt.Sprint(err)), " ")
	if len(value) > 300 {
		return value[:300]
	}
	return value
}

// CleanError returns a bounded single-line diagnostic suitable for CLI output.
func CleanError(err any) string { return cleanError(err) }

func markdownCode(value any) string {
	cleaned := strings.ReplaceAll(fmt.Sprint(value), "`", "'")
	return "`" + strings.Join(strings.Fields(cleaned), " ") + "`"
}

func ociDigest(ctx context.Context, fetcher Fetcher, source, tag string) (string, error) {
	repository := strings.TrimPrefix(source, "ghcr.io/")
	scope := url.QueryEscape("repository:" + repository + ":pull")
	document, err := fetcher.JSON(ctx, "https://ghcr.io/token?scope="+scope, nil)
	if err != nil {
		return "", err
	}
	values, ok := document.(map[string]any)
	token, tokenOK := values["token"].(string)
	if !ok || !tokenOK {
		return "", fmt.Errorf("GHCR did not issue a pull token for %s", source)
	}
	headers := make(http.Header)
	headers.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.v2+json")
	headers.Set("Authorization", "Bearer "+token)
	_, responseHeaders, err := fetcher.Get(ctx, "https://ghcr.io/v2/"+repository+"/manifests/"+url.PathEscape(tag), headers)
	if err != nil {
		return "", err
	}
	digest := responseHeaders.Get("Docker-Content-Digest")
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		return "", fmt.Errorf("GHCR returned no immutable digest for %s:%s", source, tag)
	}
	return digest, nil
}

func registryManifestDigest(ctx context.Context, fetcher Fetcher, image ImageReference, reference string) (string, error) {
	host := image.Registry
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	endpoint := "https://" + host + "/v2/" + image.Repository + "/manifests/" + url.PathEscape(reference)
	headers := make(http.Header)
	headers.Set("Accept", registryAccept)
	headers.Set("User-Agent", "docker/28.0.0 agentops-open-course-freshness")
	_, responseHeaders, err := fetcher.Get(ctx, endpoint, headers)
	if err != nil {
		var status *HTTPStatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusUnauthorized {
			return "", err
		}
		challenge := status.Headers.Get("WWW-Authenticate")
		if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
			return "", fmt.Errorf("%s: registry supplied no bearer challenge", endpoint)
		}
		parameters := make(url.Values)
		for _, match := range regexp.MustCompile(`(?i)([a-z]+)="([^"]*)"`).FindAllStringSubmatch(challenge, -1) {
			parameters.Set(match[1], match[2])
		}
		realm := parameters.Get("realm")
		parameters.Del("realm")
		parsedRealm, parseErr := url.Parse(realm)
		if parseErr != nil || parsedRealm.Scheme != "https" || parsedRealm.Host == "" {
			return "", fmt.Errorf("%s: registry supplied an unsafe token realm", endpoint)
		}
		query := parsedRealm.Query()
		for key, values := range parameters {
			for _, value := range values {
				query.Add(key, value)
			}
		}
		parsedRealm.RawQuery = query.Encode()
		document, tokenErr := fetcher.JSON(ctx, parsedRealm.String(), http.Header{"User-Agent": headers.Values("User-Agent")})
		if tokenErr != nil {
			return "", tokenErr
		}
		values, ok := document.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s: registry token response is not an object", endpoint)
		}
		token, _ := values["token"].(string)
		if token == "" {
			token, _ = values["access_token"].(string)
		}
		if token == "" {
			return "", fmt.Errorf("%s: registry supplied no pull token", endpoint)
		}
		headers.Set("Authorization", "Bearer "+token)
		_, responseHeaders, err = fetcher.Get(ctx, endpoint, headers)
		if err != nil {
			return "", err
		}
	}
	digest := responseHeaders.Get("Docker-Content-Digest")
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(digest) {
		return "", fmt.Errorf("%s: registry returned no immutable manifest digest", endpoint)
	}
	return digest, nil
}

func resolveImageFreshness(ctx context.Context, fetcher Fetcher, reference string) (imageResult, error) {
	image, ok := ParseImageReference(reference)
	if !ok {
		return imageResult{pinned: "unavailable", current: "unavailable", status: "REVIEW — mutable", authority: "unavailable"}, nil
	}
	host := image.Registry
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	authority := "https://" + host + "/v2/" + image.Repository + "/manifests/"
	pinned, err := registryManifestDigest(ctx, fetcher, image, image.Digest)
	if err != nil {
		return imageResult{}, err
	}
	if pinned != image.Digest {
		return imageResult{pinned: pinned, current: "unavailable", status: "MISMATCH", authority: authority}, nil
	}
	if image.Tag == "" {
		return imageResult{pinned: pinned, current: "no tag", status: "RESOLVES", authority: authority}, nil
	}
	current, err := registryManifestDigest(ctx, fetcher, image, image.Tag)
	if err != nil {
		return imageResult{}, err
	}
	status := "REVIEW"
	if current == image.Digest {
		status = "CURRENT"
	}
	return imageResult{pinned: pinned, current: current, status: status, authority: authority}, nil
}
