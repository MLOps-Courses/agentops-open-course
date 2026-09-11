package freshness

// ReleaseAsset is one named release artifact with an upstream-published digest.
type ReleaseAsset struct {
	Name   string
	URL    string
	Digest string
}

// StableRelease is one stable semantic release selected from an upstream feed.
type StableRelease struct {
	Tag    string
	URL    string
	Assets []ReleaseAsset
}

// HelmChart is one immutable OCI chart reference from Helmfile.
type HelmChart struct {
	Name   string
	Source string
	Digest string
}

// MiseUpdate is the actionable subset of one `mise outdated --json` record.
type MiseUpdate struct {
	Requested string
	Latest    string
}

// OllamaAssetPin is the exact evaluation archive and checksum in the workflow.
type OllamaAssetPin struct {
	Tag    string
	Name   string
	URL    string
	Digest string
}

// ImageReference is a parsed OCI image name and required digest.
type ImageReference struct {
	Registry   string
	Repository string
	Tag        string
	Digest     string
}

type imageResult struct {
	pinned    string
	current   string
	status    string
	authority string
}
