package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/freshness"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("freshness-report", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "write Markdown here instead of stdout")
	defaultRunID := os.Getenv("GITHUB_RUN_ID")
	if defaultRunID == "" {
		defaultRunID = "local"
	}
	runID := flags.String("run-id", defaultRunID, "idempotency marker")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if err == nil {
			_, _ = fmt.Fprintln(stderr, "freshness-report: unexpected positional arguments")
		}
		return 2
	}
	root, err := repositoryRoot()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "freshness report:", freshness.CleanError(err))
		return 1
	}
	document, err := freshness.Report(context.Background(), freshness.Options{
		Root: root, RunID: *runID, GeneratedAt: time.Now().UTC().Truncate(time.Second),
		Fetcher: freshness.NewHTTPFetcher(os.Getenv("GITHUB_TOKEN")),
	})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "freshness report:", freshness.CleanError(err))
		return 1
	}
	if *output == "" {
		_, err = io.WriteString(stdout, document)
	} else {
		err = os.WriteFile(*output, []byte(document), 0o600)
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "freshness report:", freshness.CleanError(err))
		return 1
	}
	return 0
}

func repositoryRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(directory, "AGENTS.md")); statErr == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("could not locate repository root")
		}
		directory = parent
	}
}
