package freshness

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/conventions"
)

var (
	githubReleases = map[string]string{
		"k3s": "k3s-io/k3s", "Ollama": "ollama/ollama", "kagent": "kagent-dev/kagent", "mise": "jdx/mise",
	}
	releaseValidation = map[string]string{
		"k3s": "check:infra + Platform", "Ollama": "doctor:model + Eval",
		"kagent": "check:infra + Platform", "mise": "install:maintainer + check",
	}
)

// Options supplies every nondeterministic report input. Tests replace Fetcher
// and MiseOutdated, so no fixture needs a model, registry, or internet access.
type Options struct {
	Fetcher      Fetcher
	MiseOutdated miseOutdatedFunc
	GeneratedAt  time.Time
	Root         string
	RunID        string
}

// Report collects local and upstream evidence into one advisory Markdown
// snapshot. Upstream failures become warnings; malformed local authorities are
// returned because the reporter cannot honestly interpret the checkout.
func Report(ctx context.Context, options Options) (string, error) {
	if options.Root == "" {
		return "", errors.New("repository root is required")
	}
	if options.RunID == "" {
		options.RunID = "local"
	}
	if options.GeneratedAt.IsZero() {
		options.GeneratedAt = time.Now().UTC().Truncate(time.Second)
	}
	if options.Fetcher == nil {
		return "", errors.New("fetcher is required")
	}
	if options.MiseOutdated == nil {
		options.MiseOutdated = runMiseOutdated
	}
	var warnings []string
	lines := []string{
		"<!-- freshness-report:" + options.RunID + " -->",
		"## Automated freshness snapshot — " + options.GeneratedAt.UTC().Format(time.DateOnly),
		"",
		"The reporter changed no pin, branch, pull request, or release; the workflow only creates or comments on this audit issue.",
		"",
	}

	proseProblems := conventions.CheckCopiedSources(options.Root)
	lines = append(lines,
		"### Copied-prose source gate", "",
		"Authority: executable repository sources through "+markdownCode("tools/bin/conventions")+". Required validation: "+markdownCode("mise run check:docs")+".", "",
	)
	if len(proseProblems) == 0 {
		// The sentence is derived, not written here. A hand-written list would let this
		// report claim a comparison CheckCopiedSources had stopped making, or omit one
		// it had started making; the checker names its own subset, in the order it ran.
		lines = append(lines, "PASS — "+conventions.CopiedSourceSummary()+" match their owners.")
	} else {
		lines = append(lines, fmt.Sprintf("REVIEW — %d copied-source mismatch(es):", len(proseProblems)))
		for _, item := range proseProblems {
			lines = append(lines, "- "+markdownCode(item.Where)+": "+item.Message)
		}
	}

	localMise, err := misePins(options.Root)
	if err != nil {
		return "", fmt.Errorf("read mise pins: %w", err)
	}
	updates, miseErr := options.MiseOutdated(ctx, options.Root)
	if miseErr != nil {
		warnings = append(warnings, "mise outdated: "+cleanError(miseErr))
	}
	lines = append(lines, "", "### mise tool pins", "",
		fmt.Sprintf("%d exact requests are owned by %s; this run resolved each against the mise registry and installed none of them.", len(localMise), markdownCode("mise.toml")), "",
		"| Tool | Pinned | Latest reported | Authority | Required validation | Result |",
		"| --- | --- | --- | --- | --- | --- |",
	)
	for _, name := range sortedMapKeys(localMise) {
		var update *MiseUpdate
		if value, ok := updates[name]; ok {
			copy := value
			update = &copy
		}
		latest, status := MiseResult(localMise[name], update, miseErr == nil)
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | [mise registry](https://mise.jdx.dev/registry.html) | %s | %s |",
			markdownCode(name), markdownCode(localMise[name]), markdownCode(latest), markdownCode(miseValidationTier(name)), status))
	}

	lines = append(lines, "", "### Go module authorities", "",
		"Application, evaluation, and repository-tool dependencies are independently locked; Dependabot proposes their updates and the module-specific check/test gates validate them.", "",
		"| Module | Authority | Required validation |",
		"| --- | --- | --- |",
		"| Agent | `agents/go/go.mod` + `agents/go/go.sum` | `check:go + test:go` |",
		"| Evaluations | `evals/go.mod` + `evals/go.sum` | `check:evals + test:evals` |",
		"| Repository tools | `tools/go.mod` + `tools/go.sum` | `check:tools + test:tools` |",
	)

	moduleStatuses, moduleWarnings := goModuleStatuses(ctx, options.Root, options.Fetcher)
	warnings = append(warnings, moduleWarnings...)
	lines = append(lines, "", "### Hand-moved Go modules", "",
		"Dependabot ignores these, so nothing else proposes their next release. Each is resolved against `proxy.golang.org`; REVIEW is a decision to schedule, not a defect.", "",
		"| Manifest | Module | Required | Latest stable | Why it moves by hand | Result |",
		"| --- | --- | --- | --- | --- | --- |",
	)
	for _, status := range moduleStatuses {
		required := status.Required
		if required == "" {
			required = "missing"
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			markdownCode(status.Manifest), markdownCode(status.Module), markdownCode(required), markdownCode(status.Latest), status.Why, status.Result))
	}
	holds, err := goCompatibilityHolds(options.Root)
	if err != nil {
		return "", fmt.Errorf("read Go compatibility holds: %w", err)
	}
	lines = append(lines, "", "### Go module compatibility holds", "",
		"A held module is not stale drift while its named owner stays at the reviewed version, the held constraint still matches, and the named acceptance remains green.", "",
		"| Manifest | Module | Held version | Owner | Constraint | Required validation | Result |",
		"| --- | --- | --- | --- | --- | --- | --- |",
	)
	for _, hold := range holds {
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |",
			markdownCode(hold.Manifest), markdownCode(hold.Module), markdownCode(hold.Version), markdownCode(hold.Owner), markdownCode(hold.Constraint), markdownCode(hold.Validator), goCompatibilityHoldStatus(options.Root, hold)))
	}
	if len(holds) == 0 {
		lines = append(lines, "| none | none | none | none | none | `check:tools + test:tools` | CURRENT |")
	}

	helmText, err := readText(filepath.Join(options.Root, "infra", "helmfile.yaml"))
	if err != nil {
		return "", fmt.Errorf("read Helmfile: %w", err)
	}
	helmVersion, charts := ParseHelmCharts(helmText)
	localReleases, err := releasePins(options.Root, helmVersion)
	if err != nil {
		return "", fmt.Errorf("read release pins: %w", err)
	}
	stableReleases := make(map[string]StableRelease)
	lines = append(lines, "", "### Latest stable upstream releases", "",
		"Drafts and tags containing alpha, beta, RC, preview, dev, or nightly markers are excluded explicitly.", "",
		"| Component | Repository pin | Latest stable | Authority | Required validation | Result |",
		"| --- | --- | --- | --- | --- | --- |",
	)
	for _, component := range []string{"k3s", "Ollama", "kagent", "mise"} {
		repository := githubReleases[component]
		document, fetchErr := options.Fetcher.JSON(ctx, "https://api.github.com/repos/"+repository+"/releases?per_page=100", nil)
		latest, ok := LatestStableRelease(document)
		if fetchErr != nil || !ok {
			message := fetchErr
			if message == nil {
				message = errors.New("release feed contained no stable semantic version")
			}
			warnings = append(warnings, component+" releases: "+cleanError(message))
			lines = append(lines, fmt.Sprintf("| %s | %s | unavailable | [GitHub releases](https://github.com/%s/releases) | %s | UNAVAILABLE |",
				component, markdownCode(localReleases[component]), repository, markdownCode(releaseValidation[component])))
			continue
		}
		stableReleases[component] = latest
		lines = append(lines, fmt.Sprintf("| %s | %s | [%s](%s) | [GitHub releases](https://github.com/%s/releases) | %s | %s |",
			component, markdownCode(localReleases[component]), markdownCode(latest.Tag), latest.URL, repository,
			markdownCode(releaseValidation[component]), ReleaseResult(component, localReleases[component], latest.Tag)))
	}

	skew, skewStatus := kubernetesSkewResult(localReleases["k3s"], localMise["kubectl"])
	lines = append(lines, "", "### Kubernetes client/server skew", "",
		"| k3s API-server pin | kubectl pin | Difference | Authority | Required validation | Result |",
		"| --- | --- | --- | --- | --- | --- |",
		fmt.Sprintf("| %s | %s | %s | [Kubernetes version-skew policy](https://kubernetes.io/releases/version-skew-policy/) | %s | %s |",
			markdownCode(localReleases["k3s"]), markdownCode(localMise["kubectl"]), markdownCode(skew), markdownCode("check:infra + Platform"), skewStatus),
	)

	evalWorkflow, err := readText(filepath.Join(options.Root, ".github", "workflows", "eval.yml"))
	if err != nil {
		return "", fmt.Errorf("read Eval workflow: %w", err)
	}
	ollamaPin, hasPin := OllamaAssetFromWorkflow(evalWorkflow)
	latestOllama, hasLatestOllama := stableReleases["Ollama"]
	assetStatus := "UNAVAILABLE"
	var upstreamAsset ReleaseAsset
	hasAsset := false
	if !hasPin {
		warnings = append(warnings, "Ollama evaluation workflow has no parseable asset and SHA-256")
	} else if !hasLatestOllama {
		warnings = append(warnings, "latest stable Ollama release is unavailable for asset comparison")
	} else {
		for _, asset := range latestOllama.Assets {
			if asset.Name == ollamaPin.Name {
				upstreamAsset, hasAsset = asset, true
				break
			}
		}
		if !hasAsset {
			warnings = append(warnings, fmt.Sprintf("Ollama release %s has no %s digest", latestOllama.Tag, ollamaPin.Name))
		} else if ollamaPin.Tag == latestOllama.Tag && ollamaPin.URL == upstreamAsset.URL && ollamaPin.Digest == upstreamAsset.Digest {
			assetStatus = "CURRENT"
		} else {
			assetStatus = "REVIEW"
		}
	}
	pinURL, pinDigest, upstreamDigest := "not found", "not found", "unavailable"
	if hasPin {
		pinURL, pinDigest = ollamaPin.URL, ollamaPin.Digest
	}
	if hasAsset {
		upstreamDigest = upstreamAsset.Digest
	}
	lines = append(lines, "", "### Ollama evaluation asset", "",
		"| Repository asset pin | Latest stable asset digest | Authority | Required validation | Result |",
		"| --- | --- | --- | --- | --- |",
		fmt.Sprintf("| %s %s | %s | [Ollama release assets](https://github.com/ollama/ollama/releases) | %s | %s |",
			markdownCode(pinURL), markdownCode(pinDigest), markdownCode(upstreamDigest), markdownCode("doctor:model + Eval"), assetStatus),
	)

	lines = append(lines, "", "### kagent Helm chart sources", "",
		"| Chart | Immutable source | Digest at reviewed tag | Digest at latest stable tag | Validation | Result |",
		"| --- | --- | --- | --- | --- | --- |",
	)
	for _, chart := range charts {
		reviewedDigest, latestDigest, status := "unavailable", "unavailable", "UNAVAILABLE"
		latestKagent, hasLatest := stableReleases["kagent"]
		if helmVersion == "" {
			warnings = append(warnings, chart.Name+" OCI source: Helmfile has no reviewed chart version")
		} else if !hasLatest {
			warnings = append(warnings, chart.Name+" OCI source: latest stable kagent release is unavailable")
		} else {
			reviewedDigest, err = ociDigest(ctx, options.Fetcher, chart.Source, helmVersion)
			if err == nil {
				latestDigest, err = ociDigest(ctx, options.Fetcher, chart.Source, strings.TrimPrefix(latestKagent.Tag, "v"))
			}
			if err != nil {
				warnings = append(warnings, chart.Name+" OCI source: "+cleanError(err))
			} else if reviewedDigest != chart.Digest {
				status = "MISMATCH"
			} else if latestKagent.Tag == "v"+helmVersion {
				status = "CURRENT"
			} else {
				status = "REVIEW"
			}
		}
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | %s | %s | %s |",
			markdownCode(chart.Name), markdownCode("oci://"+chart.Source+"@"+chart.Digest), markdownCode(reviewedDigest),
			markdownCode(latestDigest), markdownCode("check:infra + Platform"), status))
	}
	if len(charts) == 0 {
		warnings = append(warnings, "Helmfile contains no immutable kagent chart references")
		lines = append(lines, "| unavailable | unavailable | unavailable | unavailable | `check:infra + Platform` | REVIEW |")
	}

	images, err := staticImageReferences(options.Root)
	if err != nil {
		return "", fmt.Errorf("inventory static images: %w", err)
	}
	imageResults := resolveImages(ctx, options.Fetcher, images, &warnings)
	lines = append(lines, "", "### Static external image pins", "",
		"| Reference | Resolved pin | Current tag digest | Authority | Required validation | Result |",
		"| --- | --- | --- | --- | --- | --- |",
	)
	for _, reference := range sortedMapKeys(images) {
		result := imageResults[reference]
		lines = append(lines, fmt.Sprintf("| %s | %s | %s | [registry manifest](%s) from %s | %s | %s |",
			markdownCode(reference), markdownCode(result.pinned), markdownCode(result.current), result.authority,
			markdownCode(strings.Join(images[reference], ", ")), markdownCode(imageValidationTier(reference, images[reference])), result.status))
	}

	lines = append(lines, "", "### Collection warnings", "")
	if len(warnings) == 0 {
		lines = append(lines, "- None.")
	} else {
		for _, warning := range warnings {
			lines = append(lines, "- REVIEW — "+warning)
		}
	}
	lines = append(lines, "", "Treat REVIEW, MISMATCH, MISSING, UNKNOWN, and UNAVAILABLE as triage signals. This reporter never updates a pin or opens a pull request.", "")
	return strings.Join(lines, "\n"), nil
}

func resolveImages(ctx context.Context, fetcher Fetcher, images map[string][]string, warnings *[]string) map[string]imageResult {
	results := make(map[string]imageResult)
	var mutex sync.Mutex
	var wait sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for reference := range images {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				mutex.Lock()
				results[reference] = imageResult{pinned: "unavailable", current: "unavailable", status: "UNAVAILABLE", authority: "registry v2 API"}
				*warnings = append(*warnings, "image "+reference+": "+cleanError(ctx.Err()))
				mutex.Unlock()
				return
			}
			result, err := resolveImageFreshness(ctx, fetcher, reference)
			mutex.Lock()
			defer mutex.Unlock()
			if err != nil {
				*warnings = append(*warnings, "image "+reference+": "+cleanError(err))
				result = imageResult{pinned: "unavailable", current: "unavailable", status: "UNAVAILABLE", authority: "registry v2 API"}
			}
			results[reference] = result
		}()
	}
	wait.Wait()
	return results
}

func sortedMapKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
