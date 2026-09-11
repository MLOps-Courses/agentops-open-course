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

	"github.com/MLOps-Courses/agentops-open-course/tools/internal/staticserver"
)

func main() {
	directory := flag.String("directory", ".", "directory to serve")
	host := flag.String("host", "127.0.0.1", "bind address")
	port := flag.Int("port", 8001, "bind port")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		fmt.Fprintf(os.Stderr, "static-server: port must be between 1 and 65535, got %d\n", *port)
		os.Exit(2)
	}
	info, err := os.Stat(*directory)
	if err != nil || !info.IsDir() {
		fmt.Fprintf(os.Stderr, "static-server: directory %q is unavailable\n", *directory)
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	server := staticserver.Server(fmt.Sprintf("%s:%d", *host, *port), *directory, logger)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting down static server", "error", err)
		}
	}()
	logger.Info("static server listening", "address", "http://"+server.Addr, "directory", *directory)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("serving static files", "error", err)
		os.Exit(1)
	}
}
