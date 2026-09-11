package evals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultStartupTimeout  = 2 * time.Minute
	DefaultShutdownTimeout = 30 * time.Second
)

type AgentProcessConfig struct {
	Output          io.Writer
	Environment     map[string]string
	Binary          string
	Transport       string
	Entrypoint      string
	DataDir         string
	StateDir        string
	Port            int
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

// adkAPIBasePath is where ADK's `web … api` mounts every REST route. It is one
// constant because the process and container runtimes must not disagree about it.
const adkAPIBasePath = "/api"

type AgentProcess struct {
	closeErr        error
	command         *exec.Cmd
	done            chan error
	baseURL         string
	shutdownTimeout time.Duration
	closeOnce       sync.Once
}

func StartAgentProcess(ctx context.Context, config AgentProcessConfig) (*AgentProcess, error) {
	if config.Binary == "" || config.DataDir == "" || config.StateDir == "" || config.Port < 1 {
		return nil, errors.New("agent process needs a binary, data directory, state directory, and positive port")
	}
	if config.Transport != "rest" && config.Transport != "a2a" {
		return nil, fmt.Errorf("unsupported agent transport %q", config.Transport)
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = DefaultStartupTimeout
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = DefaultShutdownTimeout
	}
	arguments := []string{"a2a"}
	readinessPath := "/healthz"
	// ADK's `web … api` mounts every REST route under /api — its own startup banner
	// says so — while the A2A server serves /healthz at the root. Without this
	// prefix the readiness probe and every later request 404 against a server that
	// is running perfectly, which reads as a startup timeout.
	basePath := ""
	if config.Transport == "rest" {
		arguments = []string{"web", "-port", strconv.Itoa(config.Port), "api"}
		readinessPath = "/list-apps"
		basePath = adkAPIBasePath
	}
	command := exec.Command(config.Binary, arguments...)
	command.Env = processEnvironment(config)
	command.Stdout = config.Output
	command.Stderr = config.Output
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start agent process: %w", err)
	}
	process := &AgentProcess{
		command:         command,
		done:            make(chan error, 1),
		baseURL:         "http://127.0.0.1:" + strconv.Itoa(config.Port) + basePath,
		shutdownTimeout: config.ShutdownTimeout,
	}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	if err := process.waitReady(ctx, readinessPath, config.StartupTimeout); err != nil {
		_ = process.Close()
		return nil, err
	}
	return process, nil
}

func (p *AgentProcess) BaseURL() string {
	return p.baseURL
}

func (p *AgentProcess) Close() error {
	p.closeOnce.Do(func() {
		select {
		case err := <-p.done:
			if err != nil {
				p.closeErr = fmt.Errorf("agent process exited: %w", err)
			}
			return
		default:
		}
		if err := p.command.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
			p.closeErr = fmt.Errorf("interrupt agent process: %w", err)
			return
		}
		timer := time.NewTimer(p.shutdownTimeout)
		defer timer.Stop()
		select {
		case err := <-p.done:
			if err != nil && !expectedSignalExit(err, os.Interrupt) {
				p.closeErr = fmt.Errorf("stop agent process: %w", err)
			}
		case <-timer.C:
			if err := p.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				p.closeErr = fmt.Errorf("kill agent process after shutdown timeout: %w", err)
				return
			}
			<-p.done
		}
	})
	return p.closeErr
}

func (p *AgentProcess) waitReady(ctx context.Context, path string, timeout time.Duration) error {
	return waitForReadiness(ctx, p.done, p.baseURL, path, timeout)
}

func waitForReadiness(ctx context.Context, done <-chan error, baseURL, path string, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	probeClient := clientWithoutRedirects(&http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}, 2*time.Second)
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for agent readiness: %w", ctx.Err())
		case err := <-done:
			if err == nil {
				return errors.New("agent exited before becoming ready")
			}
			return fmt.Errorf("agent exited before becoming ready: %w", err)
		case <-deadline.C:
			return fmt.Errorf("agent did not answer %s within %s", path, timeout)
		case <-ticker.C:
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
			if err != nil {
				return fmt.Errorf("build readiness request: %w", err)
			}
			response, err := probeClient.Do(request)
			if err != nil {
				continue
			}
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}

func processEnvironment(config AgentProcessConfig) []string {
	environment := os.Environ()
	values := make(map[string]string, len(config.Environment)+5)
	for key, value := range config.Environment {
		values[key] = value
	}
	// These values define which candidate and isolated state the harness is
	// evaluating. Apply them after caller configuration, exactly as the
	// container runtime does, so an inherited .env cannot redirect the child.
	values["AGENT_ENTRYPOINT"] = config.Entrypoint
	values["AGENT_STATE_DIR"] = config.StateDir
	values["AGENT_DATA_DIR"] = config.DataDir
	values["AGENT_A2A_PORT"] = strconv.Itoa(config.Port)
	if config.Transport == "a2a" {
		// Tell the child which header to trust, and have the client send it: without
		// this pair the A2A boundary grants no principal and every guarded write
		// refuses, so "the agent asked and a named human approved" is not a claim
		// this transport could reach. The child listens on loopback and is torn down
		// with the case, so the harness really is the only thing in front of it.
		values["AGENT_TRUSTED_IDENTITY_HEADER"] = EvalIdentityHeader
	}
	// The harness owns evaluation telemetry. A child agent must never inherit or
	// receive an override that writes into its production collection pipeline.
	values["OTEL_SDK_DISABLED"] = "true"
	for key, value := range values {
		environment = replaceEnvironment(environment, key, value)
	}
	return environment
}

func replaceEnvironment(environment []string, key, value string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			filtered = append(filtered, entry)
		}
	}
	return append(filtered, prefix+value)
}

func FreePort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserve evaluation port: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		_ = listener.Close()
		return 0, fmt.Errorf("reserved port has unexpected address %T", listener.Addr())
	}
	if err := listener.Close(); err != nil {
		return 0, fmt.Errorf("release evaluation port: %w", err)
	}
	return address.Port, nil
}

func NewIsolatedState() (string, func() error, error) {
	directory, err := os.MkdirTemp("", "agentops-eval-")
	if err != nil {
		return "", nil, fmt.Errorf("create isolated evaluation state: %w", err)
	}
	cleanup := func() error {
		clean := filepath.Clean(directory)
		if filepath.Dir(clean) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(clean), "agentops-eval-") {
			return fmt.Errorf("refuse to remove unexpected state directory %s", clean)
		}
		if err := os.RemoveAll(clean); err != nil {
			return fmt.Errorf("remove isolated evaluation state: %w", err)
		}
		return nil
	}
	return directory, cleanup, nil
}

func expectedSignalExit(err error, signal os.Signal) bool {
	var exitError *exec.ExitError
	if !errors.As(err, &exitError) || exitError.ProcessState == nil {
		return false
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	want, okSignal := signal.(syscall.Signal)
	return ok && okSignal && status.Signaled() && status.Signal() == want
}
