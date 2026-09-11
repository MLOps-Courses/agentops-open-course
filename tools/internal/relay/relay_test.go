package relay

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConfigRejectsDuplicateAndInvalidPorts(t *testing.T) {
	base := Config{ListenHost: "127.0.0.1", TargetHost: "127.0.0.1", ReadyFile: "ready", Token: "token"}
	for _, ports := range [][]int{nil, {0}, {70000}, {8080, 8080}} {
		candidate := base
		candidate.Ports = ports
		if err := candidate.Validate(); err == nil {
			t.Fatalf("Validate(%v) = nil", ports)
		}
	}
}

func TestRelayPublishesReadinessAndCopiesBothDirections(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = upstream.Close() }()
	upstreamAddress, ok := upstream.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("upstream address = %T", upstream.Addr())
	}
	upstreamPort := upstreamAddress.Port
	go func() {
		connection, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		data, _ := io.ReadAll(connection)
		_, _ = connection.Write([]byte(strings.ToUpper(string(data))))
	}()

	ready := filepath.Join(t.TempDir(), "ready")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{
			ListenHost: "127.0.0.2", TargetHost: "127.0.0.1", Ports: []int{upstreamPort},
			ReadyFile: ready, Token: "proof",
		}, slog.New(slog.DiscardHandler))
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, statErr := os.Stat(ready); statErr == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readiness file was not published")
		}
		time.Sleep(5 * time.Millisecond)
	}
	content, err := os.ReadFile(ready)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "ports=") ||
		!strings.Contains(string(content), "token=proof") {
		t.Fatalf("ready = %q", content)
	}

	connection, err := net.Dial("tcp", net.JoinHostPort("127.0.0.2", strconv.Itoa(upstreamPort)))
	if err != nil {
		t.Fatal(err)
	}
	if _, writeErr := connection.Write([]byte("hello")); writeErr != nil {
		t.Fatal(writeErr)
	}
	tcpConnection, ok := connection.(*net.TCPConn)
	if !ok {
		t.Fatalf("connection = %T", connection)
	}
	if closeErr := tcpConnection.CloseWrite(); closeErr != nil {
		t.Fatal(closeErr)
	}
	response, err := io.ReadAll(connection)
	_ = connection.Close()
	if err != nil || string(response) != "HELLO" {
		t.Fatalf("response = %q, error = %v", response, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
