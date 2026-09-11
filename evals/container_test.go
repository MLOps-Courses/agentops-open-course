package evals

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeContainerCommand struct {
	done   chan error
	killed bool
	mu     sync.Mutex
}

func (c *fakeContainerCommand) Wait() error { return <-c.done }

func (c *fakeContainerCommand) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.killed = true
	return nil
}

func (c *fakeContainerCommand) wasKilled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.killed
}

func TestAgentContainerOwnsDockerLifecycleAndRuntimeIdentity(t *testing.T) {
	t.Parallel()

	command := &fakeContainerCommand{done: make(chan error, 1)}
	var started commandSpec
	var stopped commandSpec
	var readyURL string
	var readyPath string
	dependencies := containerDependencies{
		start: func(spec commandSpec) (runningCommand, error) {
			started = spec
			return command, nil
		},
		run: func(_ context.Context, spec commandSpec) error {
			stopped = spec
			command.done <- nil
			return nil
		},
		waitReady: func(_ context.Context, _ <-chan error, baseURL, path string, _ time.Duration) error {
			readyURL = baseURL
			readyPath = path
			return nil
		},
	}

	container, err := startAgentContainer(context.Background(), AgentContainerConfig{
		Engine: "docker", Image: "agentops-agent:eval",
		Transport: "rest", Entrypoint: "workflow", Port: 43123, Output: io.Discard,
		Environment: map[string]string{
			"OPENAI_API_KEY":     "should-not-appear-in-arguments",
			"OPENAI_BASE_URL":    "http://127.0.0.1:11434/v1",
			"EVAL_PRIVATE_VALUE": "must-not-enter-the-agent",
		},
	}, dependencies)
	if err != nil {
		t.Fatal(err)
	}

	if started.name != "docker" {
		t.Fatalf("engine = %q, want docker", started.name)
	}
	assertArgumentsContainInOrder(t, started.arguments, []string{
		"run", "--rm", "--name", "agentops-eval-43123", "--network", "host",
	})
	for _, expected := range []string{
		"--tmpfs", "/app/state:rw,nosuid,nodev,noexec,uid=10001,gid=10001,mode=0700",
		"agentops-agent:eval", "web", "-port", "43123", "api",
	} {
		if !slices.Contains(started.arguments, expected) {
			t.Errorf("docker arguments do not contain %q: %q", expected, started.arguments)
		}
	}
	arguments := strings.Join(started.arguments, " ")
	if strings.Contains(arguments, "should-not-appear-in-arguments") || strings.Contains(arguments, "EVAL_PRIVATE_VALUE") {
		t.Fatalf("docker arguments expose or forward private evaluator data: %s", arguments)
	}
	for _, key := range []string{
		"AGENT_ENTRYPOINT", "AGENT_STATE_DIR", "AGENT_DATA_DIR", "AGENT_A2A_BIND_HOST",
		"AGENT_A2A_PORT", "OPENAI_API_KEY", "OPENAI_BASE_URL", "OTEL_SDK_DISABLED",
	} {
		assertDockerEnvironmentKey(t, started.arguments, key)
	}
	joinedEnvironment := strings.Join(started.environment, "\n")
	for _, expected := range []string{
		"AGENT_ENTRYPOINT=workflow", "AGENT_STATE_DIR=/app/state", "AGENT_DATA_DIR=/app/data",
		"AGENT_A2A_BIND_HOST=127.0.0.1", "AGENT_A2A_PORT=43123",
		"OTEL_SDK_DISABLED=true",
	} {
		if !strings.Contains(joinedEnvironment, expected) {
			t.Errorf("container command environment is missing %q", expected)
		}
	}
	// REST probes under ADK's /api mount, exactly as the process runtime does.
	if readyURL != "http://127.0.0.1:43123/api" || readyPath != "/list-apps" {
		t.Fatalf("readiness = %q %q, want REST contract", readyURL, readyPath)
	}

	if err := container.Close(); err != nil {
		t.Fatal(err)
	}
	if stopped.name != "docker" {
		t.Fatalf("stop engine = %q, want docker", stopped.name)
	}
	if diff := strings.Join(stopped.arguments, " "); diff != "stop --timeout=30 agentops-eval-43123" {
		t.Fatalf("stop arguments = %q", diff)
	}
	if command.wasKilled() {
		t.Fatal("normal container shutdown killed the docker client")
	}
}

func TestAgentContainerA2AUsesLoopbackAndHealthReadiness(t *testing.T) {
	t.Parallel()

	command := &fakeContainerCommand{done: make(chan error, 1)}
	var started commandSpec
	var readyPath string
	dependencies := containerDependencies{
		start: func(spec commandSpec) (runningCommand, error) {
			started = spec
			return command, nil
		},
		run: func(_ context.Context, _ commandSpec) error {
			command.done <- nil
			return nil
		},
		waitReady: func(_ context.Context, _ <-chan error, _, path string, _ time.Duration) error {
			readyPath = path
			return nil
		},
	}
	container, err := startAgentContainer(context.Background(), AgentContainerConfig{
		Engine: "docker", Image: "agentops-agent:eval",
		Transport: "a2a", Entrypoint: "agent", Port: 43124,
	}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if got := started.arguments[len(started.arguments)-2:]; !slices.Equal(got, []string{"agentops-agent:eval", "a2a"}) {
		t.Fatalf("container command tail = %q", got)
	}
	if readyPath != "/healthz" {
		t.Fatalf("readiness path = %q, want /healthz", readyPath)
	}
	if err := container.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentContainerStartFailureDoesNotLaunchAnythingElse(t *testing.T) {
	t.Parallel()

	want := errors.New("docker unavailable")
	startCalls := 0
	dependencies := containerDependencies{
		start: func(commandSpec) (runningCommand, error) {
			startCalls++
			return nil, want
		},
		run: func(context.Context, commandSpec) error {
			t.Fatal("cleanup must not run for a command that never started")
			return nil
		},
		waitReady: func(context.Context, <-chan error, string, string, time.Duration) error {
			t.Fatal("readiness must not run for a command that never started")
			return nil
		},
	}
	_, err := startAgentContainer(context.Background(), AgentContainerConfig{
		Engine: "docker", Image: "agentops-agent:eval",
		Transport: "rest", Entrypoint: "agent", Port: 43125,
	}, dependencies)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want docker start failure", err)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want exactly one container attempt", startCalls)
	}
}

func TestAgentContainerReadinessFailureStopsOwnedContainer(t *testing.T) {
	t.Parallel()

	command := &fakeContainerCommand{done: make(chan error, 1)}
	want := errors.New("readiness failed")
	stopCalls := 0
	dependencies := containerDependencies{
		start: func(commandSpec) (runningCommand, error) { return command, nil },
		run: func(_ context.Context, spec commandSpec) error {
			stopCalls++
			if !slices.Equal(spec.arguments, []string{"stop", "--timeout=30", "agentops-eval-43126"}) {
				t.Fatalf("cleanup command = %q", spec.arguments)
			}
			command.done <- nil
			return nil
		},
		waitReady: func(context.Context, <-chan error, string, string, time.Duration) error { return want },
	}
	_, err := startAgentContainer(context.Background(), AgentContainerConfig{
		Image:     "agentops-agent:eval",
		Transport: "rest", Entrypoint: "agent", Port: 43126,
	}, dependencies)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want readiness failure", err)
	}
	if stopCalls != 1 {
		t.Fatalf("stop calls = %d, want one", stopCalls)
	}
}

func TestAgentContainerForceRemoveUsesIndependentContextAfterStopFailure(t *testing.T) {
	t.Parallel()

	command := &fakeContainerCommand{done: make(chan error, 1)}
	want := errors.New("docker stop timed out")
	var stopContext context.Context
	removeCalls := 0
	reusedContext := false
	dependencies := containerDependencies{
		start: func(commandSpec) (runningCommand, error) { return command, nil },
		run: func(ctx context.Context, spec commandSpec) error {
			switch spec.arguments[0] {
			case "stop":
				stopContext = ctx
				return want
			case "rm":
				removeCalls++
				reusedContext = ctx == stopContext
				command.done <- nil
				return nil
			default:
				t.Fatalf("unexpected cleanup command %q", spec.arguments)
				return nil
			}
		},
		waitReady: func(context.Context, <-chan error, string, string, time.Duration) error { return nil },
	}
	container, err := startAgentContainer(context.Background(), AgentContainerConfig{
		Image:     "agentops-agent:eval",
		Transport: "rest", Entrypoint: "agent", Port: 43127,
	}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() error = %v, want stop failure", err)
	}
	if removeCalls != 1 {
		t.Fatalf("force-remove calls = %d, want one", removeCalls)
	}
	if reusedContext {
		t.Fatal("force-remove reused the failed stop context")
	}
}

func TestAgentContainerRequiresImageEntrypointAndPort(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config AgentContainerConfig
	}{
		{name: "image", config: AgentContainerConfig{Transport: "rest", Entrypoint: "agent", Port: 1}},
		{name: "entrypoint", config: AgentContainerConfig{Image: "agent:dev", Transport: "rest", Port: 1}},
		{name: "port", config: AgentContainerConfig{Image: "agent:dev", Transport: "rest", Entrypoint: "agent"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := startAgentContainer(context.Background(), test.config, containerDependencies{}); err == nil {
				t.Fatal("missing container identity was accepted")
			}
		})
	}
}

func TestStartAgentContainerRefusesToRunAnythingWithoutAnImage(t *testing.T) {
	t.Parallel()

	// The exported entry point wires in the real engine commands, so an
	// under-specified request has to be refused before any of them can run.
	if _, err := StartAgentContainer(t.Context(), AgentContainerConfig{
		Transport: "rest", Entrypoint: "agent", Port: 43128,
	}); err == nil || !strings.Contains(err.Error(), "needs an image") {
		t.Fatalf("StartAgentContainer() error = %v, want a missing-image refusal", err)
	}
}

func TestContainerClientFactoryAttestsTheImageBeforeTrials(t *testing.T) {
	t.Parallel()

	source := testSourceEvidence()
	// The fake engine accepts exactly two commands and fails the test on anything
	// else, so a successful construction proves the factory defaulted to docker,
	// resolved the mutable tag to an immutable image ID, and ran the identity
	// check on that ID with no network and a read-only root.
	if _, err := newContainerClientFactory(t.Context(), ContainerClientFactoryConfig{
		Source: source, Image: "agent:candidate", Transport: "rest", Entrypoint: "agent",
	}, fakeContainerIdentityRunner(t, source)); err != nil {
		t.Fatalf("newContainerClientFactory() error = %v", err)
	}

	stale := source
	stale.TreeDigest = "sha256:" + strings.Repeat("c", 64)
	for name, test := range map[string]struct {
		mutate func(*ContainerClientFactoryConfig)
		runner runtimeCommand
		want   string
	}{
		"no image": {
			mutate: func(c *ContainerClientFactoryConfig) { c.Image = "" },
			want:   "needs the agent image",
		},
		"unsupported transport": {
			mutate: func(c *ContainerClientFactoryConfig) { c.Transport = "grpc" },
			want:   `unsupported container client transport "grpc"`,
		},
		"a2a entrypoint": {
			mutate: func(c *ContainerClientFactoryConfig) { c.Transport, c.Entrypoint = "a2a", "workflow" },
			want:   "serves only the agent entrypoint",
		},
		"stale image": {
			mutate: func(*ContainerClientFactoryConfig) {},
			runner: fakeContainerIdentityRunner(t, stale),
			want:   "tree_digest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := ContainerClientFactoryConfig{
				Source: source, Image: "agent:candidate", Transport: "rest", Entrypoint: "agent",
			}
			test.mutate(&config)
			runner := test.runner
			if runner == nil {
				// The validation cases must be refused before the engine is consulted,
				// so this runner would fail the test if it were ever invoked.
				runner = fakeContainerIdentityRunner(t, source)
			}
			if _, err := newContainerClientFactory(t.Context(), config, runner); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("newContainerClientFactory() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestDefaultContainerDependenciesReportChildOutcomes(t *testing.T) {
	t.Parallel()

	dependencies := defaultContainerDependencies()
	var captured strings.Builder
	command, err := dependencies.start(commandSpec{
		name: "sh", arguments: []string{"-c", "printf engine-output"}, output: &captured,
	})
	if err != nil {
		t.Fatalf("start() error = %v", err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("Wait() error = %v", err)
	}
	if got := captured.String(); got != "engine-output" {
		t.Fatalf("captured output = %q, want the child's stdout", got)
	}
	// Close only kills the engine client when it outlives the container. Killing a
	// process that already exited must not be an error, or every clean shutdown
	// would end with a spurious cleanup failure.
	if err := command.Kill(); err != nil {
		t.Fatalf("Kill(exited) error = %v, want an already-finished process to be tolerated", err)
	}

	if err := dependencies.run(t.Context(), commandSpec{
		name: "sh", arguments: []string{"-c", "exit 0"}, output: io.Discard,
	}); err != nil {
		t.Fatalf("run(success) error = %v", err)
	}
	// A failed `docker stop` has to surface, because that is what sends Close down
	// its force-removal path instead of leaving the container behind.
	if err := dependencies.run(t.Context(), commandSpec{
		name: "sh", arguments: []string{"-c", "exit 7"}, output: io.Discard,
	}); err == nil {
		t.Fatal("run(failure) error = nil, want the engine's exit status")
	}
}

func TestAgentContainerKillsAnEngineClientThatOutlivesTheContainer(t *testing.T) {
	t.Parallel()

	command := &hangingContainerCommand{done: make(chan error, 1)}
	dependencies := containerDependencies{
		start: func(commandSpec) (runningCommand, error) { return command, nil },
		// The stop command reports success while the engine client keeps running,
		// which is exactly the state a hung `docker run` leaves behind.
		run:       func(context.Context, commandSpec) error { return nil },
		waitReady: func(context.Context, <-chan error, string, string, time.Duration) error { return nil },
	}
	container, err := startAgentContainer(t.Context(), AgentContainerConfig{
		Image: "agentops-agent:eval", Transport: "rest", Entrypoint: "agent", Port: 43129,
		ShutdownTimeout: 10 * time.Millisecond,
	}, dependencies)
	if err != nil {
		t.Fatalf("startAgentContainer() error = %v", err)
	}
	if got, want := container.BaseURL(), "http://127.0.0.1:43129/api"; got != want {
		t.Fatalf("BaseURL() = %q, want %q", got, want)
	}
	err = container.Close()
	if err == nil || !strings.Contains(err.Error(), "did not exit after container shutdown") {
		t.Fatalf("Close() error = %v, want the hung client to be reported", err)
	}
	if !command.wasKilled() {
		t.Fatal("Close() left a hung engine client running")
	}
}

// hangingContainerCommand never exits on its own; releasing it is what Kill does,
// which is how a hung engine client behaves once the container is already gone.
type hangingContainerCommand struct {
	done   chan error
	killed atomic.Bool
}

func (c *hangingContainerCommand) Wait() error { return <-c.done }

func (c *hangingContainerCommand) Kill() error {
	c.killed.Store(true)
	close(c.done)
	return nil
}

func (c *hangingContainerCommand) wasKilled() bool { return c.killed.Load() }

func assertArgumentsContainInOrder(t *testing.T, arguments, expected []string) {
	t.Helper()
	position := 0
	for _, argument := range arguments {
		if position < len(expected) && argument == expected[position] {
			position++
		}
	}
	if position != len(expected) {
		t.Fatalf("arguments %q do not contain ordered subset %q", arguments, expected)
	}
}

func assertDockerEnvironmentKey(t *testing.T, arguments []string, key string) {
	t.Helper()
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == "--env" && arguments[index+1] == key {
			return
		}
	}
	t.Errorf("docker arguments do not pass environment key %q: %q", key, arguments)
}
