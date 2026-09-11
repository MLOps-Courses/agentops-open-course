package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	ListenHost string
	TargetHost string
	ReadyFile  string
	Token      string
	Ports      []int
}

func (config Config) Validate() error {
	if net.ParseIP(config.ListenHost) == nil {
		return fmt.Errorf("listen host %q is not an IP address", config.ListenHost)
	}
	if net.ParseIP(config.TargetHost) == nil {
		return fmt.Errorf("target host %q is not an IP address", config.TargetHost)
	}
	if config.ReadyFile == "" || config.Token == "" || len(config.Ports) == 0 {
		return errors.New("ready file, token, and at least one port are required")
	}
	seen := make(map[int]struct{}, len(config.Ports))
	for _, port := range config.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("port %d is outside 1..65535", port)
		}
		if _, exists := seen[port]; exists {
			return fmt.Errorf("relay ports must be unique: %d", port)
		}
		seen[port] = struct{}{}
	}
	return nil
}

func Serve(ctx context.Context, config Config, logger *slog.Logger) error {
	if err := config.Validate(); err != nil {
		return err
	}
	listeners := make([]net.Listener, 0, len(config.Ports))
	for _, port := range config.Ports {
		listener, err := net.Listen("tcp", net.JoinHostPort(config.ListenHost, strconv.Itoa(port)))
		if err != nil {
			closeListeners(listeners)
			return fmt.Errorf("listening on port %d: %w", port, err)
		}
		listeners = append(listeners, listener)
	}
	if err := writeReady(config); err != nil {
		closeListeners(listeners)
		return err
	}
	logger.Info("relay ready", "listen_host", config.ListenHost, "ports", config.Ports,
		"target_host", config.TargetHost)

	var group sync.WaitGroup
	for index, listener := range listeners {
		group.Add(1)
		go func(listener net.Listener, port int) {
			defer group.Done()
			accept(ctx, listener, config.TargetHost, port, logger)
		}(listener, config.Ports[index])
	}
	<-ctx.Done()
	closeListeners(listeners)
	group.Wait()
	return nil
}

func accept(ctx context.Context, listener net.Listener, targetHost string, port int, logger *slog.Logger) {
	for {
		client, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			logger.Warn("accepting relay connection", "port", port, "error", err)
			continue
		}
		go relay(client, targetHost, port, logger)
	}
}

func relay(client net.Conn, targetHost string, port int, logger *slog.Logger) {
	defer func() { _ = client.Close() }()
	upstream, err := net.Dial("tcp", net.JoinHostPort(targetHost, strconv.Itoa(port)))
	if err != nil {
		logger.Warn("upstream connection failed", "target_host", targetHost, "port", port, "error", err)
		return
	}
	defer func() { _ = upstream.Close() }()

	var group sync.WaitGroup
	group.Add(2)
	copyHalf := func(destination, source net.Conn) {
		defer group.Done()
		_, _ = io.Copy(destination, source)
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	go copyHalf(upstream, client)
	go copyHalf(client, upstream)
	group.Wait()
}

func writeReady(config Config) error {
	ports := slices.Clone(config.Ports)
	values := make([]string, len(ports))
	for index, port := range ports {
		values[index] = strconv.Itoa(port)
	}
	content := fmt.Sprintf("listen_host=%s\nports=%s\ntoken=%s\n",
		config.ListenHost, strings.Join(values, ","), config.Token)
	temporary := config.ReadyFile + ".tmp"
	if err := os.WriteFile(temporary, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing relay readiness file %s: %w", temporary, err)
	}
	if err := os.Rename(temporary, filepath.Clean(config.ReadyFile)); err != nil {
		return fmt.Errorf("publishing relay readiness file %s: %w", config.ReadyFile, err)
	}
	return nil
}

func closeListeners(listeners []net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}
