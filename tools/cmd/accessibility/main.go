package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/accessibility"
)

const acceptanceTimeout = 2 * time.Minute

func main() {
	flags := flag.NewFlagSet("accessibility", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	site := flags.String("site", "site", "built Hugo site directory")
	client := flags.String("client", "clients/web", "static A2A web-client directory")
	chrome := flags.String("chrome-path", os.Getenv("CHROME_PATH"), "system Chrome executable; defaults to CHROME_PATH then PATH")
	if err := flags.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "browser accessibility: unexpected arguments: %v\n", flags.Args())
		os.Exit(2)
	}

	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalContext, acceptanceTimeout)
	defer cancel()
	if err := accessibility.Run(ctx, accessibility.Config{
		SiteDirectory:   *site,
		ClientDirectory: *client,
		ChromePath:      *chrome,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "browser accessibility: %v\n", err)
		os.Exit(1)
	}
	if _, err := fmt.Fprintln(os.Stdout, "browser accessibility: documentation and web client passed"); err != nil {
		fmt.Fprintf(os.Stderr, "browser accessibility: write success output: %v\n", err)
		os.Exit(1)
	}
}
