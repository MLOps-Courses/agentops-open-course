package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/console"
	"google.golang.org/adk/v2/cmd/launcher/universal"
	"google.golang.org/adk/v2/cmd/launcher/web"
	adka2a "google.golang.org/adk/v2/cmd/launcher/web/a2a"
	"google.golang.org/adk/v2/cmd/launcher/web/api"
	"google.golang.org/adk/v2/cmd/launcher/web/triggers/eventarc"
	"google.golang.org/adk/v2/cmd/launcher/web/triggers/pubsub"
	"google.golang.org/adk/v2/cmd/launcher/web/webui"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
)

var errLauncherRuntime = errors.New("agent launcher failed; inspect sanitized logs")

const webLauncherKeyword = "web"

// launcherPlan is an already parsed ADK launcher. Parsing is deliberately
// separated from Run because ADK's public Launcher.Execute combines both, and
// assembling its Config first would open runtime state even for -help.
type launcherPlan struct {
	chosen launcher.SubLauncher
	syntax string
}

// parseLauncherPlan mirrors full.NewLauncher at the one seam ADK v2.3.0 does
// not expose: parse now, run later. The sublaunchers and their order are the
// exact constructors in the installed full launcher; their own Parse methods
// remain the authority for every flag.
func parseLauncherPlan(arguments []string) (*launcherPlan, error) {
	sublaunchers := []launcher.SubLauncher{
		console.NewLauncher(),
		web.NewLauncher(
			webui.NewLauncher(),
			adka2a.NewLauncher(),
			pubsub.NewLauncher(),
			eventarc.NewLauncher(),
			api.NewLauncher(),
		),
	}
	syntax := universal.NewLauncher(sublaunchers...).CommandLineSyntax()
	chosen := sublaunchers[0]
	remaining := arguments
	if len(arguments) > 0 {
		for _, candidate := range sublaunchers {
			if candidate.Keyword() == arguments[0] {
				chosen = candidate
				remaining = arguments[1:]
				break
			}
		}
	}
	unparsed, err := chosen.Parse(remaining)
	if err != nil {
		return &launcherPlan{chosen: chosen, syntax: syntax}, fmt.Errorf("parsing the %s launcher: %w", chosen.Keyword(), err)
	}
	if err := universal.ErrorOnUnparsedArgs(unparsed); err != nil {
		return &launcherPlan{chosen: chosen, syntax: syntax}, err
	}
	return &launcherPlan{chosen: chosen, syntax: syntax}, nil
}

func (p *launcherPlan) Run(ctx context.Context, cfg *launcher.Config) error {
	if err := p.chosen.Run(ctx, cfg); err != nil {
		// Launcher failures can contain provider response bodies. Preserve the
		// failure class for diagnosis, but never return attacker-controlled text
		// to stderr or another caller that may persist it.
		slog.ErrorContext(ctx, "agent launcher failed", "error_type", fmt.Sprintf("%T", err))
		return errLauncherRuntime
	}
	return nil
}

// runtimeConfig applies the authority bound of the selected launcher.
//
// ADK's development REST surface accepts a caller-supplied user id and its
// built-in A2A surface has no repository HTTP middleware seam. They remain
// useful read-only development surfaces, but neither identity is authentication
// and neither may carry guarded writes or durable memory writes. The standalone
// `agent a2a` command is separate from this launcher family and binds the
// gateway-verified typed principal before it permits a write.
func (p *launcherPlan) runtimeConfig(cfg config.Config) config.Config {
	if p != nil && p.chosen != nil && p.chosen.Keyword() == webLauncherKeyword {
		cfg.WritesDisabled = true
	}
	return cfg
}

func (p *launcherPlan) CommandLineSyntax() string { return p.syntax }
