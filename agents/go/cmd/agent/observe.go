package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
	adktelemetry "google.golang.org/adk/v2/telemetry"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/policy"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
)

// telemetryFlushTimeout bounds the final export at shutdown. A collector that
// is down must delay the exit briefly, not hold the process open.
const telemetryFlushTimeout = 5 * time.Second

// Who installs the OpenTelemetry tracer and logger providers.
//
// Under ADK's launcher they are the launcher's to build: it calls its own
// telemetry.New from launcher.Config.TelemetryOptions, and a second global
// provider installed here would replace the one ADK's spans were about to be
// recorded on. The standalone A2A server runs no launcher, so nobody else will
// build them and this process must — which is the same decision the Python
// track's setup_telemetry made when server.py imported the composition.
const (
	launcherInstallsProviders = false
	processInstallsProviders  = true
)

// installTelemetry brings up the parts of the observability plane this process
// owns and returns the flush that releases them, plus the options ADK's
// launcher needs to attribute its own signals.
//
// The returned flush is never nil, even on error, so a caller can register it
// before checking the error.
func installTelemetry(
	ctx context.Context,
	console io.Writer,
	governance *policy.Policy,
	build buildinfo.Info,
	ownsProviders bool,
) (func(context.Context) error, []adktelemetry.Option, error) {
	// execute applies this before dispatch, but runtime assembly is also used
	// directly by tests and embedders. Reapply the idempotent privacy boundary
	// here so neither provider-ownership path can construct an unsafe ADK tracer.
	if err := telemetry.SetContentCaptureDefaults(); err != nil {
		return noFlush, nil, fmt.Errorf("enforcing telemetry privacy defaults: %w", err)
	}

	// One resource for every signal, so a metric and the span it was recorded
	// on can never disagree about which service produced them.
	signalResource := telemetry.Resource(build)

	if err := installLogging(console, governance); err != nil {
		return noFlush, nil, err
	}

	// Metrics before providers and before the launcher: ADK v2.3.0 builds no
	// MeterProvider at all, so without this every instrument the agent records
	// itself on resolves to a no-op and nothing reaches Prometheus.
	flushMetrics, err := telemetry.InstallMeterProvider(ctx, signalResource)
	if err != nil {
		return flushMetrics, nil, fmt.Errorf("installing the meter provider: %w", err)
	}

	if !ownsProviders {
		return flushMetrics, []adktelemetry.Option{adktelemetry.WithResource(signalResource)}, nil
	}
	flushProviders, err := installProviders(ctx, signalResource)
	flush := joinFlushes(flushMetrics, flushProviders)
	if err != nil {
		return flush, nil, err
	}
	return flush, nil, nil
}

// installLogging makes every record this process writes trace-correlated and
// redacted before console persistence, then optionally exports a separately
// redacted copy when an OTLP log endpoint is configured.
func installLogging(console io.Writer, governance *policy.Policy) error {
	handler, err := telemetry.NewHandler(telemetry.Config{
		// Standard error, never standard output: on the MCP stdio transport
		// stdout carries the protocol, and a log line in the middle of it is a
		// framing error rather than a cosmetic problem.
		Console: slog.NewTextHandler(console, &slog.HandlerOptions{Level: slog.LevelInfo}),
		// The persisted redactor, not the boundary one: terminals, containers,
		// CI capture, and collectors all retain output after this call returns.
		Redact: governance.RedactPersistedValue,
		// Decided once, here, rather than read from the environment inside the
		// handler, so a test can decide differently without touching the
		// process environment.
		ExportLogs: telemetry.LogsConfigured(),
	})
	if err != nil {
		return fmt.Errorf("building the log handler: %w", err)
	}
	slog.SetDefault(slog.New(handler))
	standard, err := telemetry.NewSanitizingWriter(console, governance.RedactPersistedValue)
	if err != nil {
		return fmt.Errorf("building the standard log writer: %w", err)
	}
	// ADK v2.3.0 still uses package log on several launcher and runner paths;
	// route those records through the same durable-sink policy as slog.
	log.SetOutput(standard)
	return nil
}

// installProviders builds ADK's tracer and logger providers and installs them
// globally, for a process that runs no launcher.
//
// With no endpoint configured it installs nothing: a provider built here would
// record every span and then drop it, and the account-free local path stays
// silent rather than paying for telemetry nobody is listening for.
func installProviders(
	ctx context.Context, signalResource *resource.Resource,
) (func(context.Context) error, error) {
	if !telemetry.ExportConfigured() {
		return noFlush, nil
	}
	providers, err := adktelemetry.New(ctx, adktelemetry.WithResource(signalResource))
	if err != nil {
		return noFlush, fmt.Errorf("building the OpenTelemetry providers: %w", err)
	}
	providers.SetGlobalOtelProviders()
	return providers.Shutdown, nil
}

// flushTelemetry releases the observability plane, reporting a failure to the
// log rather than to the process exit code.
//
// The flush runs on a context detached from the one that triggered it, because
// a shutdown context is already canceled by the time it arrives here — and an
// exporter given a canceled context flushes for exactly zero seconds, dropping
// the records that describe the shutdown.
//
// The failure is logged and not returned. A collector that is unreachable at
// shutdown is an observability fault, and a non-zero exit would escalate it
// into a deployment fault: Kubernetes reads a non-zero exit after SIGTERM as a
// failed pod and restarts it. The console handler is the sink that still works
// when the collector is the thing that is broken, which is why it is safe to
// report it there and let the process exit on the merit of the work it did.
func flushTelemetry(ctx context.Context, flush func(context.Context) error) {
	release, cancel := context.WithTimeout(context.WithoutCancel(ctx), telemetryFlushTimeout)
	defer cancel()
	if err := flush(release); err != nil {
		slog.WarnContext(release, "releasing the observability plane failed", "error", err)
	}
}

// noFlush is what an installer returns when it installed nothing, so a caller
// can always register the result without a nil check.
func noFlush(context.Context) error { return nil }

// joinFlushes runs every flush and keeps all their failures: one broken
// exporter must not hide another's failure or stop it from being tried.
func joinFlushes(flushes ...func(context.Context) error) func(context.Context) error {
	return func(ctx context.Context) error {
		problems := make([]error, 0, len(flushes))
		for _, flush := range flushes {
			problems = append(problems, flush(ctx))
		}
		return errors.Join(problems...)
	}
}
