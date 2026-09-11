package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/coursecertificate"
	"github.com/MLOps-Courses/agentops-open-course/tools/internal/courseevidence"
)

func main() {
	if err := execute(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "course certificate:", err)
		os.Exit(1)
	}
}

func execute(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("course-certificate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	name := flags.String("name", "", "name to print on the certificate")
	tier := flags.String("tier", "", "capstone tier claimed: bronze, silver, or gold")
	manifestPath := flags.String("manifest", "", "completion manifest to render; defaults to the one `create` writes")
	output := flags.String("output", "", "certificate output path; defaults beside the manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("course-certificate does not accept positional arguments")
	}
	root, err := courseevidence.RepositoryRoot()
	if err != nil {
		return err
	}
	if *manifestPath == "" {
		*manifestPath = courseevidence.DefaultManifestPath(root)
	}
	if *output == "" {
		*output = defaultOutput(*manifestPath)
	}
	svg, digest, err := render(root, *manifestPath, *name, *tier)
	if err != nil {
		return err
	}
	if err = write(*output, svg); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(stdout, "course certificate: wrote", courseevidence.DisplayPath(*output)); err != nil {
		return err
	}
	// The digest is printed as well as drawn, because it is the string a reader
	// retypes into `sha256sum` and a terminal is easier to copy from than an SVG.
	_, err = fmt.Fprintln(stdout, "course certificate: manifest sha256", digest)
	return err
}

func render(root, manifestPath, name, tier string) (string, string, error) {
	claimed, err := coursecertificate.ParseTier(tier)
	if err != nil {
		return "", "", err
	}
	version, err := courseVersion(root)
	if err != nil {
		return "", "", err
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("reading the completion manifest %s: %w", manifestPath, err)
	}
	manifest, err := courseevidence.Decode(content)
	if err != nil {
		return "", "", err
	}
	// The digest is taken over the exact bytes on disk, not over a re-encoding of
	// the decoded manifest. That is what makes `sha256sum` on the same file agree
	// with the certificate, which is the only re-derivation step a reader has.
	digest := coursecertificate.Digest(content)
	svg, err := coursecertificate.Render(coursecertificate.Input{
		Name: name, Tier: claimed, CourseVersion: version, ManifestDigest: digest, Manifest: manifest,
	})
	if err != nil {
		return "", "", err
	}
	return svg, digest, nil
}

func write(output, svg string) error {
	if err := os.MkdirAll(filepath.Dir(output), 0o750); err != nil {
		return fmt.Errorf("creating the certificate directory: %w", err)
	}
	if err := os.WriteFile(output, []byte(svg), 0o600); err != nil {
		return fmt.Errorf("writing the certificate %s: %w", output, err)
	}
	return nil
}

// defaultOutput keeps the certificate next to the manifest it renders, so both
// land in the same gitignored directory and neither becomes a tracked claim about
// a checkout it no longer describes.
func defaultOutput(manifestPath string) string {
	extension := filepath.Ext(manifestPath)
	return strings.TrimSuffix(manifestPath, extension) + "-certificate.svg"
}

// courseVersion reads the repository VERSION file, which `check:release-metadata`
// already holds as the single authority for the course version. Accepting a
// --version flag instead would let a certificate name a release that never
// existed.
func courseVersion(root string) (string, error) {
	content, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return "", fmt.Errorf("reading the course VERSION: %w", err)
	}
	version := strings.TrimSpace(string(content))
	if version == "" {
		return "", errors.New("the course VERSION file is empty")
	}
	return version, nil
}
