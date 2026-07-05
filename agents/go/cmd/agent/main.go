// Command agent runs the AgentOps reference agent (Go track) via the ADK launcher.
//
// The `full` launcher exposes every mode via its command-line keywords: `console`
// (the default) and `web`, whose sub-servers are `webui` (browser UI), `api` (REST),
// and `a2a` — e.g. `web webui`. Running `web` alone errors, as it needs a sub-server.
// Needs GOOGLE_API_KEY in the env (plus GOOGLE_GENAI_USE_VERTEXAI=true for a Vertex key).
//
// Observability (Ch. 7): the launcher initializes OpenTelemetry and reads the standard
// OTEL_EXPORTER_OTLP_ENDPOINT env var, so setting it exports traces to a collector with no
// code change. Pass launcher.Config.TelemetryOptions for finer control.
package main

import (
	"context"
	"log"
	"os"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/full"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/agent"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/config"
)

func main() {
	ctx := context.Background()

	a, store, err := agent.New(ctx, config.Load())
	if err != nil {
		log.Fatalf("failed to build agent: %v", err)
	}
	defer func() { _ = store.Close() }()

	cfg := &launcher.Config{AgentLoader: adkagent.NewSingleLoader(a)}

	l := full.NewLauncher()
	if err := l.Execute(ctx, cfg, os.Args[1:]); err != nil {
		log.Fatalf("run failed: %v\n\n%s", err, l.CommandLineSyntax())
	}
}
