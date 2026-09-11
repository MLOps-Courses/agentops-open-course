package evals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultContainerEngine = "docker"
	containerStateMount    = "/app/state:rw,nosuid,nodev,noexec,uid=10001,gid=10001,mode=0700"
)

// AgentContainerConfig describes one isolated evaluation agent container.
// The evaluator owns the container lifecycle; callers provide no pre-existing
// service for this mode.
type AgentContainerConfig struct {
	Output          io.Writer
	Environment     map[string]string
	Engine          string
	Image           string
	Transport       string
	Entrypoint      string
	Port            int
	StartupTimeout  time.Duration
	ShutdownTimeout time.Duration
}

type commandSpec struct {
	output      io.Writer
	name        string
	arguments   []string
	environment []string
}

type runningCommand interface {
	Wait() error
	Kill() error
}

type execRunningCommand struct {
	command *exec.Cmd
}

func (c *execRunningCommand) Wait() error { return c.command.Wait() }

func (c *execRunningCommand) Kill() error {
	if err := c.command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

type containerDependencies struct {
	start     func(commandSpec) (runningCommand, error)
	run       func(context.Context, commandSpec) error
	waitReady func(context.Context, <-chan error, string, string, time.Duration) error
}

func defaultContainerDependencies() containerDependencies {
	return containerDependencies{
		start: func(spec commandSpec) (runningCommand, error) {
			command := exec.Command(spec.name, spec.arguments...)
			command.Env = spec.environment
			command.Stdout = spec.output
			command.Stderr = spec.output
			if err := command.Start(); err != nil {
				return nil, err
			}
			return &execRunningCommand{command: command}, nil
		},
		run: func(ctx context.Context, spec commandSpec) error {
			command := exec.CommandContext(ctx, spec.name, spec.arguments...)
			command.Env = spec.environment
			command.Stdout = spec.output
			command.Stderr = spec.output
			return command.Run()
		},
		waitReady: waitForReadiness,
	}
}

type AgentContainer struct {
	closeErr        error
	command         runningCommand
	output          io.Writer
	dependencies    containerDependencies
	done            chan error
	engine          string
	name            string
	baseURL         string
	environment     []string
	shutdownTimeout time.Duration
	closeOnce       sync.Once
}

// StartAgentContainer starts exactly the requested image. It never falls back
// to a host binary when the engine, image, or container startup fails.
func StartAgentContainer(ctx context.Context, config AgentContainerConfig) (*AgentContainer, error) {
	return startAgentContainer(ctx, config, defaultContainerDependencies())
}

func startAgentContainer(
	ctx context.Context, config AgentContainerConfig, dependencies containerDependencies,
) (*AgentContainer, error) {
	if config.Engine == "" {
		config.Engine = defaultContainerEngine
	}
	if config.Image == "" || config.Entrypoint == "" || config.Port < 1 {
		return nil, errors.New("agent container needs an image, entrypoint, and positive port")
	}
	if config.Transport != "rest" && config.Transport != "a2a" {
		return nil, fmt.Errorf("unsupported agent container transport %q", config.Transport)
	}
	if config.Transport == "a2a" && config.Entrypoint != "agent" {
		return nil, errors.New("the deployed A2A contract serves only the agent entrypoint")
	}
	if config.StartupTimeout <= 0 {
		config.StartupTimeout = DefaultStartupTimeout
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = DefaultShutdownTimeout
	}
	if dependencies.start == nil || dependencies.run == nil || dependencies.waitReady == nil {
		return nil, errors.New("agent container command dependencies are incomplete")
	}

	name := "agentops-eval-" + strconv.Itoa(config.Port)
	environment, environmentKeys := containerEnvironment(config)
	arguments := []string{
		"run", "--rm", "--name", name, "--network", "host",
		"--tmpfs", containerStateMount,
	}
	for _, key := range environmentKeys {
		// Passing only the name keeps credentials out of the process arguments;
		// Docker reads each value from its own inherited environment.
		arguments = append(arguments, "--env", key)
	}
	arguments = append(arguments, config.Image)
	readinessPath := "/healthz"
	// Same /api prefix as the process runtime: ADK's `web … api` mounts the REST
	// routes under it, and the A2A server does not.
	basePath := ""
	if config.Transport == "rest" {
		arguments = append(arguments, "web", "-port", strconv.Itoa(config.Port), "api")
		readinessPath = "/list-apps"
		basePath = adkAPIBasePath
	} else {
		arguments = append(arguments, "a2a")
	}
	spec := commandSpec{
		name: config.Engine, arguments: arguments, environment: environment, output: config.Output,
	}
	command, err := dependencies.start(spec)
	if err != nil {
		return nil, fmt.Errorf("start agent container: %w", err)
	}
	container := &AgentContainer{
		command: command, done: make(chan error, 1), dependencies: dependencies,
		engine: config.Engine, name: name,
		baseURL:     "http://127.0.0.1:" + strconv.Itoa(config.Port) + basePath,
		environment: environment, output: config.Output, shutdownTimeout: config.ShutdownTimeout,
	}
	go func() {
		container.done <- command.Wait()
		close(container.done)
	}()
	if err := dependencies.waitReady(ctx, container.done, container.baseURL, readinessPath, config.StartupTimeout); err != nil {
		return nil, errors.Join(err, container.Close())
	}
	return container, nil
}

func (c *AgentContainer) BaseURL() string { return c.baseURL }

func (c *AgentContainer) Close() error {
	c.closeOnce.Do(func() {
		select {
		case err := <-c.done:
			if err != nil {
				c.closeErr = fmt.Errorf("agent container exited: %w", err)
			}
			return
		default:
		}

		cleanupTimeout := c.shutdownTimeout + 5*time.Second
		stopCtx, cancelStop := context.WithTimeout(context.Background(), cleanupTimeout)
		stopSpec := commandSpec{
			name: c.engine, arguments: []string{"stop", "--timeout=" + strconv.Itoa(int(c.shutdownTimeout.Seconds())), c.name},
			environment: c.environment, output: c.output,
		}
		stopErr := c.dependencies.run(stopCtx, stopSpec)
		cancelStop()
		if stopErr != nil {
			stopErr = fmt.Errorf("stop agent container: %w", stopErr)
			removeSpec := commandSpec{
				name: c.engine, arguments: []string{"rm", "--force", c.name},
				environment: c.environment, output: c.output,
			}
			// A timed-out stop context is already canceled. Force removal needs its
			// own bound or Docker never receives the fallback command.
			removeCtx, cancelRemove := context.WithTimeout(context.Background(), cleanupTimeout)
			removeErr := c.dependencies.run(removeCtx, removeSpec)
			cancelRemove()
			if removeErr != nil {
				c.closeErr = errors.Join(stopErr, fmt.Errorf("remove agent container after stop failure: %w", removeErr))
			} else {
				c.closeErr = stopErr
			}
		}

		timer := time.NewTimer(c.shutdownTimeout)
		defer timer.Stop()
		select {
		case err := <-c.done:
			if err != nil {
				c.closeErr = errors.Join(c.closeErr, fmt.Errorf("agent container command exited: %w", err))
			}
		case <-timer.C:
			if err := c.command.Kill(); err != nil {
				c.closeErr = errors.Join(c.closeErr, fmt.Errorf("kill hung container client: %w", err))
			}
			<-c.done
			c.closeErr = errors.Join(c.closeErr, errors.New("container client did not exit after container shutdown"))
		}
	})
	return c.closeErr
}

func containerEnvironment(config AgentContainerConfig) ([]string, []string) {
	values := make(map[string]string, len(config.Environment)+6)
	for key, value := range config.Environment {
		if isAgentContainerVariable(key) {
			values[key] = value
		}
	}
	values["AGENT_ENTRYPOINT"] = config.Entrypoint
	values["AGENT_STATE_DIR"] = "/app/state"
	values["AGENT_DATA_DIR"] = "/app/data"
	values["AGENT_A2A_BIND_HOST"] = "127.0.0.1"
	values["AGENT_A2A_HOST"] = "localhost"
	values["AGENT_A2A_PORT"] = strconv.Itoa(config.Port)
	// The evaluator owns OTel evidence. The child container may not export to
	// the agent's collection path, even when the host environment requests it.
	values["OTEL_SDK_DISABLED"] = "true"

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := os.Environ()
	for _, key := range keys {
		environment = replaceEnvironment(environment, key, values[key])
	}
	return environment, keys
}

func isAgentContainerVariable(key string) bool {
	if strings.HasPrefix(key, "AGENT_") {
		return true
	}
	switch key {
	case "OPENAI_BASE_URL", "OPENAI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_ENTERPRISE",
		"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION":
		return true
	default:
		return false
	}
}
