package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/relay"
)

type ports []int

func (values *ports) String() string { return fmt.Sprint([]int(*values)) }

func (values *ports) Set(raw string) error {
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("port %q is not an integer: %w", raw, err)
	}
	*values = append(*values, parsed)
	return nil
}

func main() {
	var configuredPorts ports
	listenHost := flag.String("listen-host", "", "Docker bridge gateway address")
	targetHost := flag.String("target-host", "127.0.0.1", "host-loopback target address")
	readyFile := flag.String("ready-file", "", "atomic readiness file")
	token := flag.String("token", "", "readiness ownership token")
	flag.Var(&configuredPorts, "port", "port to relay; repeat for multiple ports")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := relay.Serve(ctx, relay.Config{
		ListenHost: *listenHost, TargetHost: *targetHost, Ports: configuredPorts,
		ReadyFile: *readyFile, Token: *token,
	}, logger); err != nil {
		logger.Error("loopback relay failed", "error", err)
		os.Exit(1)
	}
}
