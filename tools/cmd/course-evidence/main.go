package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/courseevidence"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	root, err := courseevidence.RepositoryRoot()
	if err != nil {
		fail(err)
	}
	config := courseevidence.DefaultConfig(root)
	switch os.Args[1] {
	case "create":
		set := flag.NewFlagSet("create", flag.ExitOnError)
		output := set.String("output", courseevidence.DefaultManifestPath(root), "manifest output path")
		_ = set.Parse(os.Args[2:])
		if err := courseevidence.Create(context.Background(), config, *output); err != nil {
			fail(err)
		}
		fmt.Println("course evidence: wrote", courseevidence.DisplayPath(*output))
		fmt.Println("course evidence: wrote", courseevidence.DisplayPath(courseevidence.SummaryPath(*output)))
	case "verify":
		set := flag.NewFlagSet("verify", flag.ExitOnError)
		// The same default as `create`, so the documented pair works on a clean
		// tree. A positional argument still overrides it, which is how a reviewer
		// verifies a manifest someone handed them.
		manifest := set.String("manifest", courseevidence.DefaultManifestPath(root), "manifest path to verify")
		_ = set.Parse(os.Args[2:])
		if set.NArg() > 1 {
			fail(errors.New("verify accepts at most one manifest path"))
		}
		if set.NArg() == 1 {
			*manifest = set.Arg(0)
		}
		if *manifest == "" {
			fail(errors.New("verify needs a manifest path"))
		}
		revision, err := courseevidence.Verify(context.Background(), config, *manifest)
		if err != nil {
			fail(err)
		}
		fmt.Println("course evidence: verified", revision)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: course-evidence create [--output path] | verify [--manifest path | <manifest>]")
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "course evidence:", err)
	os.Exit(1)
}
