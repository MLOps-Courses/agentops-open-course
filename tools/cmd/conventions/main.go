package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/conventions"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) < 1 {
		printUsage(stderr)
		return 2
	}
	root, err := repositoryRoot()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "conventions:", err)
		return 1
	}
	var problems []conventions.Problem
	switch arguments[0] {
	case "docs":
		if len(arguments) != 1 {
			printUsage(stderr)
			return 2
		}
		problems = conventions.CheckDocs(root)
	case "rendered":
		if len(arguments) > 2 {
			printUsage(stderr)
			return 2
		}
		directory := "site"
		if len(arguments) == 2 {
			directory = arguments[1]
		}
		problems = conventions.CheckRendered(root, directory)
	case "skills":
		if len(arguments) != 1 {
			printUsage(stderr)
			return 2
		}
		problems = conventions.CheckSkills(root)
	case "release-metadata":
		if len(arguments) > 3 {
			printUsage(stderr)
			return 2
		}
		expectedTag := ""
		expectedRepository := ""
		if len(arguments) == 2 {
			expectedTag = arguments[1]
		}
		if len(arguments) == 3 {
			expectedTag = arguments[1]
			expectedRepository = arguments[2]
		}
		metadata, err := conventions.ValidateReleaseMetadata(root, expectedTag, expectedRepository)
		if err != nil {
			problems = append(problems, conventions.Problem{Where: "release metadata", Message: err.Error()})
		} else {
			_, _ = fmt.Fprintf(stdout, "release metadata: v%s (%s) is consistent\n", metadata.Version, metadata.Date)
		}
	default:
		printUsage(stderr)
		return 2
	}
	for _, item := range problems {
		_, _ = fmt.Fprintln(stderr, item.String())
	}
	if len(problems) > 0 {
		return 1
	}
	return 0
}

func printUsage(writer io.Writer) {
	_, _ = fmt.Fprintln(writer, "usage: conventions <docs|rendered|skills|release-metadata> [tag [repository]]")
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
