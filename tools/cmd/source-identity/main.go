package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/sourceidentity"
)

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "source-identity:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("source-identity", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", ".", "source directory inside the Git repository")
	modeValue := flags.String("mode", string(sourceidentity.Development), "identity mode: development or release")
	field := flags.String("field", "", "print one field: display, revision, tree-digest, mode, dirty, or shallow")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("source-identity does not accept positional arguments")
	}
	identity, err := sourceidentity.Resolve(ctx, *root, sourceidentity.Mode(*modeValue))
	if err != nil {
		return err
	}
	if *field == "" {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(identity)
	}
	var value any
	switch *field {
	case "display":
		value = identity.Display
	case "revision":
		if identity.Revision == "" {
			return errors.New("dirty development source has no revision")
		}
		value = identity.Revision
	case "tree-digest":
		value = identity.TreeDigest
	case "mode":
		value = identity.Mode
	case "dirty":
		value = identity.Dirty
	case "shallow":
		value = identity.Shallow
	default:
		return fmt.Errorf("unknown source identity field %q", *field)
	}
	_, err = fmt.Fprintln(stdout, value)
	return err
}
