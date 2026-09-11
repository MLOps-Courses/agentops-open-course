// Command kagent-invoke calls the optional declarative incident specialist
// through kagent's agent-as-MCP endpoint.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.36.0"
	adktelemetry "google.golang.org/adk/v2/telemetry"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/kagentinterop"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

const (
	defaultEndpoint      = "http://127.0.0.1:8083/mcp"
	defaultTimeout       = 2 * time.Minute
	telemetryStopTimeout = 5 * time.Second
	clientServiceName    = "agentops-kagent-client"
)

type client interface {
	Invoke(context.Context, string, string) (kagentinterop.Result, error)
	Close() error
}

type dialer func(context.Context, kagentinterop.Config) (client, error)

type options struct {
	endpoint  string
	task      string
	contextID string
	timeout   time.Duration
}

func main() { os.Exit(realMain()) }

func realMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdown, err := installClientTelemetry(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "kagent-invoke: initialize telemetry: %v\n", err)
		return 1
	}

	runErr := run(ctx, os.Args[1:], os.Stdout, os.Stderr, func(
		ctx context.Context,
		cfg kagentinterop.Config,
	) (client, error) {
		return kagentinterop.Dial(ctx, cfg)
	})
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryStopTimeout)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		// An unavailable collector is an observability fault, not a reason to
		// turn a successful specialist answer into a failed invocation.
		_, _ = fmt.Fprintf(os.Stderr, "kagent-invoke: flush telemetry: %v\n", err)
	}
	if runErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "kagent-invoke: %v\n", runErr)
		return 1
	}
	return 0
}

func run(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	stderr io.Writer,
	dial dialer,
) error {
	selected, err := parseOptions(args, stderr)
	if err != nil {
		return err
	}
	remote, err := dial(ctx, kagentinterop.Config{
		Endpoint: selected.endpoint,
		Timeout:  selected.timeout,
	})
	if err != nil {
		return err
	}
	result, invokeErr := remote.Invoke(ctx, selected.task, selected.contextID)
	closeErr := remote.Close()
	if err := errors.Join(invokeErr, closeErr); err != nil {
		return err
	}
	if err := writeResult(stdout, result); err != nil {
		return fmt.Errorf("write the structured result: %w", err)
	}
	return nil
}

func parseOptions(args []string, stderr io.Writer) (options, error) {
	selected := options{}
	flags := flag.NewFlagSet("kagent-invoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&selected.endpoint, "endpoint", defaultEndpoint, "kagent Streamable HTTP MCP endpoint")
	flags.DurationVar(&selected.timeout, "timeout", defaultTimeout, "per-request HTTP timeout")
	flags.StringVar(&selected.task, "task", "", "bounded incident investigation task (required)")
	flags.StringVar(&selected.contextID, "context-id", "", "opaque context_id returned by a prior invocation")
	if err := flags.Parse(args); err != nil {
		return options{}, err
	}
	if flags.NArg() != 0 {
		return options{}, fmt.Errorf("unexpected positional arguments: %q", flags.Args())
	}
	if strings.TrimSpace(selected.task) == "" {
		return options{}, errors.New("--task is required")
	}
	return selected, nil
}

func writeResult(destination io.Writer, result kagentinterop.Result) error {
	// JSON keeps the opaque context_id distinct from model text so callers can
	// resume without parsing the specialist's natural-language response.
	return json.NewEncoder(destination).Encode(result)
}

func installClientTelemetry(ctx context.Context) (func(context.Context) error, error) {
	build, err := buildinfo.Current()
	if err != nil {
		return noShutdown, fmt.Errorf("resolve build information: %w", err)
	}
	clientResource, err := clientTelemetryResource(build)
	if err != nil {
		return noShutdown, err
	}
	shutdownMetrics, err := telemetry.InstallMeterProvider(ctx, clientResource)
	if err != nil {
		return noShutdown, err
	}
	if !telemetry.ExportConfigured() {
		return shutdownMetrics, nil
	}
	// This command uses ADK's public builder only as an OTLP provider factory;
	// it never constructs or runs an ADK agent. Its two repository-owned spans
	// carry only a bounded operation and constant status, as the kagentinterop
	// telemetry test proves. Applying the agent runtime's ADK risk gate here
	// would erase this deliberately content-free comparison telemetry.
	providers, err := adktelemetry.New(ctx, adktelemetry.WithResource(clientResource))
	if err != nil {
		return noShutdown, errors.Join(
			fmt.Errorf("build OpenTelemetry providers: %w", err),
			shutdownMetrics(ctx),
		)
	}
	providers.SetGlobalOtelProviders()
	return func(ctx context.Context) error {
		return errors.Join(shutdownMetrics(ctx), providers.Shutdown(ctx))
	}, nil
}

func clientTelemetryResource(build buildinfo.Info) (*resource.Resource, error) {
	clientResource, err := resource.Merge(
		telemetry.Resource(build),
		resource.NewSchemaless(semconv.ServiceName(clientServiceName)),
	)
	if err != nil {
		return nil, fmt.Errorf("build client telemetry resource: %w", err)
	}
	return clientResource, nil
}

func noShutdown(context.Context) error { return nil }
