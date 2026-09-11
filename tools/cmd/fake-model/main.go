package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/fakemodel"
)

func main() {
	host := flag.String("host", "127.0.0.1", "bind address")
	port := flag.Int("port", 11434, "bind port")
	// Without a script the fixture keeps its single-reply behavior, which is what
	// scripts/smoke-host.sh drives and must keep working unchanged.
	scriptPath := flag.String("script", "", "trajectory script driving scripted tool calls")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "fake-model: port must be between 1 and 65535, got %d\n", *port)
		os.Exit(2)
	}
	var script *fakemodel.Script
	if *scriptPath != "" {
		loaded, err := fakemodel.LoadScript(*scriptPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fake-model: %v\n", err)
			os.Exit(2)
		}
		script = loaded
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	server := fakemodel.Server(fmt.Sprintf("%s:%d", *host, *port), logger, script)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting down fake model", "error", err)
		}
	}()
	logger.Info("fake model listening", "address", "http://"+server.Addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serving fake model", "error", err)
		os.Exit(1)
	}
}
