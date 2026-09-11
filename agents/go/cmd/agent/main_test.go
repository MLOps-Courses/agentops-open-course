package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/cmd/launcher"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/a2aserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/buildinfo"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/compose"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/config"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/mcpserver"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/memory"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/resilience"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/state"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/telemetry"
	agenttools "github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

type failingSubLauncher struct{ failure error }

func (f failingSubLauncher) Keyword() string { return "failing" }
func (f failingSubLauncher) Parse(arguments []string) ([]string, error) {
	return arguments, nil
}
func (f failingSubLauncher) CommandLineSyntax() string { return "agent failing" }
func (f failingSubLauncher) SimpleDescription() string { return "test failure" }
func (f failingSubLauncher) Run(context.Context, *launcher.Config) error {
	return f.failure
}

type inspectingSubLauncher struct {
	inspect func(*launcher.Config) error
}

func (inspectingSubLauncher) Keyword() string { return webLauncherKeyword }
func (inspectingSubLauncher) Parse(arguments []string) ([]string, error) {
	return arguments, nil
}
func (inspectingSubLauncher) CommandLineSyntax() string { return "agent web" }
func (inspectingSubLauncher) SimpleDescription() string { return "inspect web runtime" }
func (l inspectingSubLauncher) Run(_ context.Context, cfg *launcher.Config) error {
	return l.inspect(cfg)
}

type namedTool string

func (t namedTool) Name() string      { return string(t) }
func (namedTool) Description() string { return "test tool" }
func (namedTool) IsLongRunning() bool { return false }

// These tests are offline: no model is called and no port is opened. What they
// prove is the wiring — that each repository subcommand reaches its own package
// without the launcher parsing it, that the conversational surface holds every
// tool its instruction claims, and that sessions are actually persistent.

// repositoryDataset is the committed dataset, relative to this package.
const repositoryDataset = "../../../data"

// isolatedEnvironment clears the AGENT_ and provider variables a developer's
// shell may carry, so a test judges the configuration it set rather than the
// one that happened to be exported.
func isolatedEnvironment(t *testing.T) {
	t.Helper()

	for _, setting := range (config.Config{}).Describe() {
		// t.Setenv records the original value and restores it on cleanup;
		// unsetting straight afterwards is what makes the variable genuinely
		// absent rather than explicitly empty, which the loader deliberately
		// treats as two different states.
		t.Setenv(setting.Variable, "")
		if err := os.Unsetenv(setting.Variable); err != nil {
			t.Fatalf("clearing %s: %v", setting.Variable, err)
		}
	}
	t.Setenv(config.EnvDataDir, repositoryDataset)
	t.Setenv(config.EnvStateDir, t.TempDir())
}

// offlineConfig is the configuration every wiring test assembles against: the
// committed dataset, a disposable state directory, and nothing that reaches a
// network.
func offlineConfig(t *testing.T, entrypoint config.Entrypoint) config.Config {
	t.Helper()

	return config.Config{
		Entrypoint:         entrypoint,
		ModelProvider:      config.ProviderOpenAICompatible,
		Model:              "qwen3:4b-instruct",
		OpenAIBaseURL:      "http://127.0.0.1:11434/v1",
		OpenAIAPIKey:       "local-ollama",
		DataDir:            repositoryDataset,
		StateDir:           t.TempDir(),
		EmbeddingsURL:      "http://127.0.0.1:11434",
		EmbeddingModel:     "nomic-embed-text",
		EmbeddingTimeout:   config.Seconds(120),
		DrainTimeout:       config.Seconds(10),
		ModelTimeout:       config.Seconds(60),
		ToolTimeout:        config.Seconds(30),
		MaxRetries:         2,
		RetryBackoff:       config.Seconds(0.5),
		A2ABindHost:        "127.0.0.1",
		A2AHost:            "localhost",
		A2AProtocol:        "http",
		A2APort:            8080,
		A2AMaxLLMCalls:     12,
		SanitizeToolOutput: true,
	}
}

func recoveredTestState(t *testing.T, cfg config.Config) recoveredState {
	t.Helper()
	recovered, err := recoverRuntimeState(cfg.StateDir)
	if err != nil {
		t.Fatalf("recoverRuntimeState() error = %v, want nil", err)
	}
	return recovered
}

// keepDefaultLogger restores the process logger a test's telemetry install
// replaced, so one test's observability plane is not another's.
func keepDefaultLogger(t *testing.T) {
	t.Helper()

	previous := slog.Default()
	standardOutput := log.Writer()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		log.SetOutput(standardOutput)
	})
}

func TestLauncherRuntimeFailureDoesNotReturnAProviderBody(t *testing.T) {
	keepDefaultLogger(t)

	const untrusted = "provider body at https://provider.invalid: password=SYNTHETIC_DO_NOT_USE_123456"
	var logs bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	plan := &launcherPlan{
		chosen: failingSubLauncher{failure: errors.New(untrusted)},
		syntax: "agent failing",
	}

	err := plan.Run(t.Context(), &launcher.Config{})
	if err == nil {
		t.Fatal("Run() error = nil, want an opaque launcher failure")
	}
	if strings.Contains(err.Error(), untrusted) || strings.Contains(err.Error(), "SYNTHETIC_DO_NOT_USE") {
		t.Fatalf("Run() returned an untrusted provider body: %q", err)
	}
	if strings.Contains(logs.String(), untrusted) || strings.Contains(logs.String(), "SYNTHETIC_DO_NOT_USE") {
		t.Fatalf("launcher log retained an untrusted provider body: %q", logs.String())
	}
	if !strings.Contains(logs.String(), "error_type") {
		t.Fatalf("launcher log = %q, want a diagnostic error type", logs.String())
	}
}

// TestADKWebLauncherForcesTheWriteKillSwitch covers the network surfaces ADK
// owns internally. Its REST requests accept a caller-supplied user id and its
// built-in A2A path has no repository identity middleware, so neither may carry
// the write-capable runtime used by the explicitly local console.
func TestADKWebLauncherForcesTheWriteKillSwitch(t *testing.T) {
	for name, testCase := range map[string]struct {
		arguments      []string
		writesDisabled bool
	}{
		"local console retains configured writes": {
			arguments: []string{"console"},
		},
		"web REST is read only": {
			arguments:      []string{"web", "api"},
			writesDisabled: true,
		},
		"web A2A is read only": {
			arguments:      []string{"web", "a2a"},
			writesDisabled: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := parseLauncherPlan(testCase.arguments)
			if err != nil {
				t.Fatalf("parseLauncherPlan(%v) error = %v, want nil", testCase.arguments, err)
			}
			secured := plan.runtimeConfig(config.Config{})
			if secured.WritesDisabled != testCase.writesDisabled {
				t.Errorf("WritesDisabled = %t, want %t", secured.WritesDisabled, testCase.writesDisabled)
			}
		})
	}
}

// TestADKWebRuntimeRefusesAnActionBeforeConfirmation exercises the production
// run path, not only launcherPlan.runtimeConfig. The fake sublauncher opens no
// port; it inspects the exact plugin configuration handed to ADK web.
func TestADKWebRuntimeRefusesAnActionBeforeConfirmation(t *testing.T) {
	isolatedEnvironment(t)
	keepDefaultLogger(t)

	plan := &launcherPlan{
		chosen: inspectingSubLauncher{inspect: func(cfg *launcher.Config) error {
			if len(cfg.PluginConfig.Plugins) != 1 {
				return fmt.Errorf("web runtime has %d policy plugins, want 1", len(cfg.PluginConfig.Plugins))
			}
			callback := cfg.PluginConfig.Plugins[0].BeforeToolCallback()
			if callback == nil {
				return errors.New("web runtime has no before-tool policy callback")
			}
			refusal, err := callback(nil, namedTool(agenttools.RestartServiceToolName), map[string]any{})
			if err != nil {
				return fmt.Errorf("before-tool policy callback: %w", err)
			}
			message, ok := refusal["error"].(string)
			if !ok || !strings.Contains(message, config.EnvWritesDisabled) {
				return fmt.Errorf("web action refusal = %v, want the write kill switch", refusal)
			}
			return nil
		}},
		syntax: "agent web",
	}

	if err := run(t.Context(), plan, io.Discard); err != nil {
		t.Fatalf("run(web) error = %v, want a read-only runtime", err)
	}
}

// TestConfigCheckIsDispatchedBeforeTheLauncher covers the subcommand
// `mise run config:check` invokes.
//
// It has to be intercepted first: the launcher routes an unrecognized first
// argument to its default sublauncher, the console, which would report
// "config:check" as an unparsed flag instead of running the check.
func TestConfigCheckIsDispatchedBeforeTheLauncher(t *testing.T) {
	isolatedEnvironment(t)

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:check"}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("execute() = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr = %q, want it empty on a valid configuration", errOut.String())
	}
	for _, want := range []string{config.EnvEntrypoint, config.EnvModel, config.EnvOpenAIAPIKey} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report never mentions %s", want)
		}
	}
	// The secret is masked, which is the property that makes this command safe
	// to paste into an issue.
	if strings.Contains(out.String(), "local-ollama") {
		t.Error("the report disclosed the API key")
	}
	if !strings.Contains(out.String(), config.SecretMask) {
		t.Error("the report never shows a masked secret")
	}
}

func TestConfigExampleSubcommandRendersAndChecksTheGeneratedFile(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:example"}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("render execute() = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
	generated, err := config.RenderEnvExample()
	if err != nil {
		t.Fatalf("RenderEnvExample(): %v", err)
	}
	if out.String() != string(generated) {
		t.Fatal("config:example output differs from config.RenderEnvExample()")
	}

	out.Reset()
	errOut.Reset()
	path := filepath.Join("..", "..", "..", "..", ".env.example")
	if code := execute(t.Context(), []string{"config:example", "--check", path}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("check execute() = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
}

func TestConfigExampleSubcommandRejectsAnInvalidMode(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:example", "--unknown"}, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute() = %d, want %d", code, config.ExitInvalid)
	}
	if !strings.Contains(errOut.String(), "config:example [--check PATH | --write PATH]") {
		t.Fatalf("stderr = %q, want exact usage guidance", errOut.String())
	}
}

func TestVersionReportsTheValidatedDevelopmentIdentityWithoutRuntimeStartup(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{versionCommand}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("execute(version) = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
	var got buildinfo.Info
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode version output: %v", err)
	}
	if err := buildinfo.Validate(got); err != nil {
		t.Fatalf("version output is not a valid build identity: %v", err)
	}
	if got.Mode != buildinfo.Development || got.Version != buildinfo.DevelopmentVersion ||
		got.SourceIdentity != buildinfo.DevelopmentIdentity || !got.Dirty {
		t.Fatalf("version output = %#v, want the explicit go-run development identity", got)
	}
}

// TestConfigCheckReportsAnInvalidConfiguration pins the non-zero exit and the
// error channel: the task that runs this is a gate, and a gate that exits zero
// on a broken configuration is not a gate.
func TestConfigCheckReportsAnInvalidConfiguration(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv(config.EnvMaxRetries, "not-a-number")

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:check"}, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute() = %d, want %d", code, config.ExitInvalid)
	}
	if !strings.Contains(errOut.String(), config.EnvMaxRetries) {
		t.Errorf("stderr = %q, want it to name the bad variable", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want the report to stay off the success channel", out.String())
	}
}

// TestContentCaptureIsPinnedBeforeAnythingElse covers the course invariant that
// telemetry content stays private by default.
//
// It runs on the config:check path deliberately: the defaults are pinned before
// the dispatch, so every mode of the binary gets them — including the ones that
// never build a model. ADK reads its switch through a sync.Once, so a call made
// any later would silently not apply.
func TestContentCaptureIsPinnedBeforeAnythingElse(t *testing.T) {
	isolatedEnvironment(t)
	for _, variable := range []string{
		telemetry.EnvADKCaptureMessageContent, telemetry.EnvGenAICaptureMessageContent,
	} {
		t.Setenv(variable, "")
		if err := os.Unsetenv(variable); err != nil {
			t.Fatalf("clearing %s: %v", variable, err)
		}
	}

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:check"}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("execute() = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
	for _, variable := range []string{
		telemetry.EnvADKCaptureMessageContent, telemetry.EnvGenAICaptureMessageContent,
	} {
		if got := os.Getenv(variable); got != telemetry.ContentCaptureDisabled {
			t.Errorf("%s = %q, want %q", variable, got, telemetry.ContentCaptureDisabled)
		}
	}
}

// TestUnsafeADKTraceSamplingIsForcedOffBeforeAnythingElse is the fail-closed
// boundary for the pinned ADK. ADK v2.3.0 records serialized tool arguments,
// tool results, and raw exception text even when both content-capture switches
// are false, so an operator's sampler choice cannot safely re-enable spans.
func TestUnsafeADKTraceSamplingIsForcedOffBeforeAnythingElse(t *testing.T) {
	for name, testCase := range map[string]struct {
		captureValue string
		wantCapture  string
		wantError    string
		wantSampler  string
		wantExit     int
		captureSet   bool
	}{
		"unset risk acceptance overrides an existing sampler": {
			wantCapture: "false",
			wantSampler: "always_off",
		},
		"literal false overrides an existing sampler": {
			captureSet:   true,
			captureValue: "false",
			wantCapture:  "false",
			wantSampler:  "always_off",
		},
		"malformed acceptance fails closed": {
			captureSet:   true,
			captureValue: "TRUE",
			wantCapture:  "false",
			wantError:    "must be the literal \"true\" or \"false\"",
			wantExit:     config.ExitInvalid,
			wantSampler:  "always_off",
		},
		"literal true preserves the operator sampler": {
			captureSet:   true,
			captureValue: "true",
			wantCapture:  "true",
			wantSampler:  "always_on",
		},
	} {
		t.Run(name, func(t *testing.T) {
			isolatedEnvironment(t)
			t.Setenv(telemetry.EnvADKCaptureMessageContent, "")
			if err := os.Unsetenv(telemetry.EnvADKCaptureMessageContent); err != nil {
				t.Fatalf("clearing %s: %v", telemetry.EnvADKCaptureMessageContent, err)
			}
			if testCase.captureSet {
				t.Setenv(telemetry.EnvADKCaptureMessageContent, testCase.captureValue)
			}
			t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

			var out, errOut bytes.Buffer
			if code := execute(t.Context(), []string{"config:check"}, &out, &errOut); code != testCase.wantExit {
				t.Fatalf("execute() = %d, want %d; stderr: %s", code, testCase.wantExit, errOut.String())
			}
			if testCase.wantError != "" {
				if !strings.Contains(errOut.String(), telemetry.EnvADKCaptureMessageContent) ||
					!strings.Contains(errOut.String(), testCase.wantError) {
					t.Errorf("stderr = %q, want the variable and %q", errOut.String(), testCase.wantError)
				}
			}
			if got := os.Getenv(telemetry.EnvADKCaptureMessageContent); got != testCase.wantCapture {
				t.Errorf("%s = %q, want %q", telemetry.EnvADKCaptureMessageContent, got, testCase.wantCapture)
			}
			if got := os.Getenv("OTEL_TRACES_SAMPLER"); got != testCase.wantSampler {
				t.Errorf("OTEL_TRACES_SAMPLER = %q, want %q", got, testCase.wantSampler)
			}
		})
	}
}

// TestAnOperatorChoiceOfContentCaptureSurvives pins the explicit-risk half of
// the rule: exact true survives so a learner can run the synthetic trace lab.
func TestAnOperatorChoiceOfContentCaptureSurvives(t *testing.T) {
	isolatedEnvironment(t)
	t.Setenv(telemetry.EnvADKCaptureMessageContent, "true")

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"config:check"}, &out, &errOut); code != config.ExitValid {
		t.Fatalf("execute() = %d, want %d; stderr: %s", code, config.ExitValid, errOut.String())
	}
	if got := os.Getenv(telemetry.EnvADKCaptureMessageContent); got != "true" {
		t.Errorf("%s = %q, want the operator's own value", telemetry.EnvADKCaptureMessageContent, got)
	}
}

// TestLauncherHelpDoesNotTouchInterruptedState is the parse-before-runtime
// boundary. A help request must not recover, lock, publish, or migrate state:
// it only describes flags and exits.
func TestLauncherHelpDoesNotTouchInterruptedState(t *testing.T) {
	isolatedEnvironment(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(filepath.Join(stateDir, ".restore-staged.unexplained"), 0o750); err != nil {
		t.Fatalf("plant interrupted-restore residue: %v", err)
	}
	t.Setenv(config.EnvStateDir, stateDir)

	before, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state before help: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"web", "-help"}, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute(web -help) = %d, want %d", code, config.ExitInvalid)
	}
	after, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatalf("read state after help: %v", err)
	}
	if !reflect.DeepEqual(directoryEntryNames(before), directoryEntryNames(after)) {
		t.Fatalf("state entries after help = %v, want unchanged %v",
			directoryEntryNames(after), directoryEntryNames(before))
	}
}

// TestLauncherRecoversBeforePublishingAnyDatabase makes the ordering
// observable. If runtime.db were created before recovery, it would be an
// unjournaled live file and the committed journal's exact-inventory check would
// refuse to clean up. Successful cleanup followed by runtime.db publication
// therefore proves recovery won the race.
func TestLauncherRecoversBeforePublishingAnyDatabase(t *testing.T) {
	isolatedEnvironment(t)
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o750); err != nil {
		t.Fatalf("create state fixture: %v", err)
	}
	const transactionID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const stagingDir = ".restore-staged." + transactionID
	const quarantineDir = ".restore-quarantine." + transactionID
	live := filepath.Join(stateDir, "existing.db")
	payload := []byte("existing generation")
	if err := os.WriteFile(live, payload, 0o600); err != nil {
		t.Fatalf("write live generation: %v", err)
	}
	digest := sha256.Sum256(payload)
	journal := map[string]any{
		"format_version": 1,
		"transaction_id": transactionID,
		"phase":          "committed",
		"staging_dir":    stagingDir,
		"quarantine_dir": quarantineDir,
		"old_inventory":  []any{},
		"new_inventory": []any{map[string]any{
			"filename": "existing.db", "sha256": hex.EncodeToString(digest[:]), "size_bytes": len(payload),
		}},
	}
	encoded, err := json.Marshal(journal)
	if err != nil {
		t.Fatalf("encode restore journal: %v", err)
	}
	for _, name := range []string{stagingDir, quarantineDir} {
		if err := os.Mkdir(filepath.Join(stateDir, name), 0o750); err != nil {
			t.Fatalf("create restore residue %s: %v", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stateDir, ".restore-journal.json"), encoded, 0o600); err != nil {
		t.Fatalf("write restore journal: %v", err)
	}
	t.Setenv(config.EnvStateDir, stateDir)

	// A parsed web command with no selected sub-surface returns before binding a
	// port, but only after the ordinary runtime and session store are built.
	var out, errOut bytes.Buffer
	if code := execute(t.Context(), []string{"web"}, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute(web) = %d, want %d", code, config.ExitInvalid)
	}
	for _, name := range []string{
		".restore-journal.json", stagingDir, quarantineDir,
	} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("restore residue %s survived launcher recovery: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, a2aserver.SessionDatabaseName)); err != nil {
		t.Errorf("session database was not published after recovery: %v", err)
	}
}

func directoryEntryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestTheRuntimeAssemblesEveryPlane is the wiring guarantee this binary exists
// for: one value carries the model, the policy plane, the tools, the knowledge
// and recall surface, and the compositions, and the launcher and A2A paths both
// serve it.
//
// It replaces the placeholder assertion this test file used to carry, which
// recorded that the four memory-owned tools were unset and that the binary
// refused to start because of it. They are set now, so the fact worth pinning
// is the opposite one.
func TestTheRuntimeAssemblesEveryPlane(t *testing.T) {
	keepDefaultLogger(t)

	cfg := offlineConfig(t, config.EntrypointAgent)
	assembled, err := newAgentRuntime(
		t.Context(), cfg, recoveredTestState(t, cfg), io.Discard, launcherInstallsProviders,
	)
	if err != nil {
		t.Fatalf("newAgentRuntime() error = %v, want nil", err)
	}
	t.Cleanup(func() { assembled.close(t.Context()) })

	root, err := assembled.compositions.RootAgent()
	if err != nil {
		t.Fatalf("RootAgent() error = %v, want nil", err)
	}
	if root.Name() != compose.AgentName {
		t.Errorf("RootAgent() = %q, want %q", root.Name(), compose.AgentName)
	}
	// The persistent memory service is what ADK's own load_memory and
	// preload_memory resolve against; a nil one would leave them answering from
	// an in-memory store that dies with the process.
	if assembled.memories.Service() == nil {
		t.Error("the runtime carries no memory service")
	}
	if assembled.store == nil {
		t.Error("the runtime carries no dataset store")
	}
	if assembled.build.Version == "" {
		t.Error("the runtime resolved no version to attribute its telemetry to")
	}
}

// TestTheToolSurfaceCarriesEveryToolTheInstructionNames pins the four tools the
// memory package owns onto the surface the compositions draw from.
//
// The root instruction tells the model to call all four. A surface missing one
// would produce an agent that answers a documented capability with a
// hallucination, which is why compose.New refuses the wiring by name rather
// than starting.
func TestTheToolSurfaceCarriesEveryToolTheInstructionNames(t *testing.T) {
	t.Parallel()

	surface, memories := offlineToolSurface(t, config.EntrypointAgent)

	for name, built := range map[string]tool.Tool{
		compose.GetRunbookToolName:     surface.GetRunbook,
		compose.SearchRunbooksToolName: surface.SearchRunbooks,
	} {
		if built == nil {
			t.Fatalf("the surface has no %s tool", name)
		}
		if built.Name() != name {
			t.Errorf("tool name = %q, want %q", built.Name(), name)
		}
	}

	// Registration order is part of the contract: it is the order the agent
	// binds them in and the order the Python track's MEMORY_TOOLS listed.
	recall := memories.MemoryTools()
	want := []string{compose.RecallIncidentContextToolName, compose.SaveIncidentNoteToolName}
	got := make([]string, 0, len(recall))
	for _, built := range recall {
		got = append(got, built.Name())
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MemoryTools() = %v, want %v", got, want)
	}
}

// TestTheMCPServerServesExactlyTheReadAllowlist covers the second process this
// binary runs.
//
// The six names are a least-privilege allowlist shared with the client, and the
// server refuses to start on a seventh — so this asserts both that the wiring
// picks the right six and that it picks them in the order the allowlist names.
func TestTheMCPServerServesExactlyTheReadAllowlist(t *testing.T) {
	t.Parallel()

	surface, _ := offlineToolSurface(t, config.EntrypointAgent)
	server, err := mcpserver.New(mcpserver.Config{
		Probe:   func(context.Context) error { return nil },
		Tools:   slices.Concat(surface.ReadTools(), surface.KnowledgeTools()),
		Version: buildinfo.DevelopmentVersion,
	})
	if err != nil {
		t.Fatalf("mcpserver.New() error = %v, want nil", err)
	}
	if got := server.ToolNames(); !reflect.DeepEqual(got, compose.MCPReadToolNames()) {
		t.Errorf("ToolNames() = %v, want %v", got, compose.MCPReadToolNames())
	}
	// The two guarded writes stay in the agent process, where a human can
	// actually approve them.
	for _, forbidden := range []string{surface.RestartService.Name(), surface.ResolveIncident.Name()} {
		if slices.Contains(server.ToolNames(), forbidden) {
			t.Errorf("the MCP surface serves the guarded write %q", forbidden)
		}
	}
}

// TestTheMCPProcessCarriesNoResiliencePolicy pins the guard that process uses.
//
// Retries, deadlines and the circuit breaker belong to the caller: the ADK
// client supplies its own timeout, and a server that retried on the client's
// behalf would double the load on a dependency that is already struggling.
func TestTheMCPProcessCarriesNoResiliencePolicy(t *testing.T) {
	t.Parallel()

	attempts := 0
	refuse := errors.New("the dependency is down")
	err := passThroughGuard(t.Context(), "list_incidents", func(context.Context) error {
		attempts++
		return refuse
	})
	if !errors.Is(err, refuse) {
		t.Errorf("passThroughGuard() error = %v, want the call's own failure", err)
	}
	if attempts != 1 {
		t.Errorf("the call ran %d times, want exactly one attempt", attempts)
	}
}

// TestTheMCPProcessIsBuiltFromTheEnvironmentAlone covers the `agent mcp`
// dispatch up to the point it would start serving.
//
// The transport is the one thing that changes between `mise run mcp` and
// `mise run mcp:http`, and it changes through the environment the deployment
// sets — never through a flag this binary would have to re-declare.
func TestTheMCPProcessIsBuiltFromTheEnvironmentAlone(t *testing.T) {
	keepDefaultLogger(t)
	t.Setenv(mcpserver.EnvTransport, string(mcpserver.TransportStreamableHTTP))
	t.Setenv(mcpserver.EnvPort, "8000")

	cfg := offlineConfig(t, config.EntrypointAgent)
	server, flush, err := newMCPServer(t.Context(), cfg, io.Discard)
	if err != nil {
		t.Fatalf("newMCPServer() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if flushErr := flush(t.Context()); flushErr != nil {
			t.Errorf("flush() error = %v, want nil", flushErr)
		}
	})
	if got := server.Transport(); got != mcpserver.TransportStreamableHTTP {
		t.Errorf("Transport() = %q, want %q", got, mcpserver.TransportStreamableHTTP)
	}
	if got := server.Address(); got != "127.0.0.1:8000" {
		t.Errorf("Address() = %q, want the address the deployment pinned", got)
	}
	if got := server.ToolNames(); !reflect.DeepEqual(got, compose.MCPReadToolNames()) {
		t.Errorf("ToolNames() = %v, want %v", got, compose.MCPReadToolNames())
	}

	// An unsupported transport is one named failure at the one place a learner
	// can supply one, rather than a server that quietly serves something else.
	t.Setenv(mcpserver.EnvTransport, "websocket")
	if _, _, err := newMCPServer(t.Context(), cfg, io.Discard); !errors.Is(err, mcpserver.ErrInvalidOptions) {
		t.Errorf("newMCPServer() error = %v, want ErrInvalidOptions", err)
	}
}

// TestTheA2AProcessOwnsFirstBoot runs the startup sequence the deployed
// contract depends on, over a real state directory and no port.
//
// The order is the guarantee: an interrupted state transaction is recovered
// before anything reads or publishes, then the incident database is published,
// then both schemas are created — and only a server that finished all of it
// reports ready.
func TestTheA2AProcessOwnsFirstBoot(t *testing.T) {
	keepDefaultLogger(t)

	cfg := offlineConfig(t, config.EntrypointAgent)
	server, assembled, err := newA2AServer(t.Context(), cfg, recoveredTestState(t, cfg), io.Discard)
	if err != nil {
		t.Fatalf("newA2AServer() error = %v, want nil", err)
	}
	t.Cleanup(func() { assembled.close(t.Context()) })

	// Construction is a pure wiring decision: the task store is opened during
	// startup, after the state preflight, and there is nothing before then.
	if server.TaskStore() != nil {
		t.Error("the task store was opened before the startup sequence ran")
	}
	if got := server.Options().StateDir; got != cfg.StateDir {
		t.Errorf("Options().StateDir = %q, want %q", got, cfg.StateDir)
	}
	if got := server.AgentCard().Name; got != a2aserver.CardName {
		t.Errorf("AgentCard().Name = %q, want %q", got, a2aserver.CardName)
	}

	if err := server.Start(t.Context()); err != nil {
		t.Fatalf("Start() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if closeErr := server.Close(); closeErr != nil {
			t.Errorf("Close() error = %v, want nil", closeErr)
		}
	})
	// The three files first boot is responsible for: the published incident
	// database, ADK's session schema, and this package's task schema.
	for _, name := range []string{"incidents.db", a2aserver.SessionDatabaseName, a2aserver.TaskDatabaseName} {
		if _, err := os.Stat(filepath.Join(cfg.StateDir, name)); err != nil {
			t.Errorf("Stat(%s) = %v, want the file first boot publishes", name, err)
		}
	}

	// Readiness is an observation over that state, served without a port.
	request := httptest.NewRequest(http.MethodGet, a2aserver.ReadinessPath, nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request.WithContext(t.Context()))
	if recorder.Code != http.StatusOK {
		t.Errorf("GET %s = %d (%s), want %d",
			a2aserver.ReadinessPath, recorder.Code, recorder.Body.String(), http.StatusOK)
	}
}

// TestTheA2AServerRefusesAnEntrypointItCannotServe pins the one composition the
// public card describes.
//
// The card advertises the conversational agent's two skills, so serving the
// workflow or the coordinator behind it would publish a contract the process
// does not honor.
func TestTheA2AServerRefusesAnEntrypointItCannotServe(t *testing.T) {
	keepDefaultLogger(t)

	cfg := offlineConfig(t, config.EntrypointWorkflow)
	if _, _, err := newA2AServer(
		t.Context(), cfg, recoveredTestState(t, cfg), io.Discard,
	); !errors.Is(err, a2aserver.ErrIncompleteConfig) {
		t.Errorf("newA2AServer() error = %v, want ErrIncompleteConfig", err)
	}
}

// TestTheLauncherConfigPublishesEveryPlane covers what ADK's launcher family is
// handed: both agents, the persistent session and memory stores, exactly one
// policy plugin, and the resource its own signals are attributed to.
func TestTheLauncherConfigPublishesEveryPlane(t *testing.T) {
	keepDefaultLogger(t)

	cfg := offlineConfig(t, config.EntrypointAgent)
	assembled, err := newAgentRuntime(
		t.Context(), cfg, recoveredTestState(t, cfg), io.Discard, launcherInstallsProviders,
	)
	if err != nil {
		t.Fatalf("newAgentRuntime() error = %v, want nil", err)
	}
	t.Cleanup(func() { assembled.close(t.Context()) })

	launcherConfig, err := assembled.launcherConfig()
	if err != nil {
		t.Fatalf("launcherConfig() error = %v, want nil", err)
	}
	published := slices.Sorted(slices.Values(launcherConfig.AgentLoader.ListAgents()))
	want := slices.Sorted(slices.Values([]string{compose.AgentName, compose.ReportAgentName}))
	if !reflect.DeepEqual(published, want) {
		t.Errorf("ListAgents() = %v, want %v", published, want)
	}
	if launcherConfig.SessionService == nil {
		t.Error("the launcher is handed no session service")
	}
	if launcherConfig.MemoryService == nil {
		t.Error("the launcher is handed no memory service")
	}
	// One plugin, attached once, at the application boundary: a second copy
	// would double-count tokens and double-redact every turn.
	if len(launcherConfig.PluginConfig.Plugins) != 1 {
		t.Errorf("PluginConfig.Plugins = %d, want exactly the governance plugin",
			len(launcherConfig.PluginConfig.Plugins))
	}
	if len(launcherConfig.TelemetryOptions) != 1 {
		t.Errorf("TelemetryOptions = %d, want exactly the resource", len(launcherConfig.TelemetryOptions))
	}

	// The session store the launcher gets is migrated: ADK creates its tables
	// lazily and the console would otherwise fail on the first turn.
	if _, err := launcherConfig.SessionService.Create(t.Context(), &session.CreateRequest{
		AppName: compose.AgentName, UserID: "engineer",
	}); err != nil {
		t.Errorf("Create() error = %v, want a migrated session store", err)
	}
}

// TestServerSubcommandsRefuseFlagsTheyDoNotOwn covers the dispatch of the two
// long-running surfaces.
//
// Both are configured entirely by environment variables, exactly as the Python
// track configures them, so an argument is a mistake worth naming rather than
// something to ignore. The failure also proves the dispatch reached the
// subcommand at all: the launcher would have answered with its own syntax.
func TestServerSubcommandsRefuseFlagsTheyDoNotOwn(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		wantHint  string
		arguments []string
	}{
		"mcp": {arguments: []string{mcpCommand, "--transport=stdio"}, wantHint: mcpserver.EnvTransport},
		"a2a": {arguments: []string{a2aCommand, "--port=8080"}, wantHint: config.EnvEntrypoint},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var out, errOut bytes.Buffer
			if code := execute(t.Context(), testCase.arguments, &out, &errOut); code != config.ExitInvalid {
				t.Fatalf("execute(%v) = %d, want %d", testCase.arguments, code, config.ExitInvalid)
			}
			if !strings.Contains(errOut.String(), testCase.wantHint) {
				t.Errorf("stderr = %q, want it to name %s", errOut.String(), testCase.wantHint)
			}
			// The launcher's syntax belongs to the launcher's failures alone;
			// printing it here would send an operator to the wrong manual.
			if strings.Contains(errOut.String(), "console") {
				t.Errorf("stderr = %q, want no launcher syntax on a subcommand failure", errOut.String())
			}
		})
	}
}

// TestStateSubcommandIsDispatchedBeforeTheLauncher covers the backup and
// restore command the host scripts and the Kubernetes CronJob drive.
func TestStateSubcommandIsDispatchedBeforeTheLauncher(t *testing.T) {
	isolatedEnvironment(t)

	stateDir := t.TempDir()
	backupRoot := filepath.Join(t.TempDir(), "snapshots")
	t.Setenv(config.EnvStateDir, stateDir)
	// A snapshot is taken of SQLite databases, so there has to be one: opening
	// the session store the way the launcher path does is what creates it.
	cfg := config.Config{StateDir: stateDir}
	if _, err := newSessionService(cfg, recoveredTestState(t, cfg)); err != nil {
		t.Fatalf("newSessionService() error = %v, want nil", err)
	}

	var out, errOut bytes.Buffer
	arguments := []string{state.Command, state.BackupSubcommand, "--backup-root", backupRoot}
	if code := execute(t.Context(), arguments, &out, &errOut); code != config.ExitValid {
		t.Fatalf("execute(%v) = %d, want %d; stderr: %s", arguments, code, config.ExitValid, errOut.String())
	}
	entries, err := os.ReadDir(backupRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ReadDir(%s) = %d entries, %v; want exactly one published snapshot", backupRoot, len(entries), err)
	}

	// An unknown subcommand is the state command's own error, not the
	// launcher's report of an unparsed flag.
	out.Reset()
	errOut.Reset()
	if code := execute(t.Context(), []string{state.Command, "vacuum"}, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute(state vacuum) = %d, want %d", code, config.ExitInvalid)
	}
	for _, want := range []string{state.BackupSubcommand, state.RestoreSubcommand} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("stderr = %q, want it to name the %s subcommand", errOut.String(), want)
		}
	}
}

// TestStateEnvironmentSeparatesUnsetFromUnparseable covers the retention
// setting a CronJob passes through the process environment.
//
// A pointer rather than a string, because a typo must not silently fall back to
// the default retention and start deleting snapshots on a different schedule.
func TestStateEnvironmentSeparatesUnsetFromUnparseable(t *testing.T) {
	for _, variable := range []string{state.EnvBackupKeep, state.EnvBackupTimestamp} {
		t.Setenv(variable, "")
		if err := os.Unsetenv(variable); err != nil {
			t.Fatalf("clearing %s: %v", variable, err)
		}
	}
	if environment := stateEnvironment(); environment.Keep != nil {
		t.Errorf("Keep = %q, want nil for an unset %s", *environment.Keep, state.EnvBackupKeep)
	}

	t.Setenv(state.EnvBackupKeep, "not-a-number")
	t.Setenv(state.EnvBackupTimestamp, "20260101T000000Z")
	environment := stateEnvironment()
	if environment.Keep == nil || *environment.Keep != "not-a-number" {
		t.Errorf("Keep = %v, want the operator's own value verbatim", environment.Keep)
	}
	if environment.Timestamp != "20260101T000000Z" {
		t.Errorf("Timestamp = %q, want the pinned stamp", environment.Timestamp)
	}
}

// TestTelemetryInstallsTheTraceStampingHandler pins the join key the whole
// observability chapter is built on: without the stamping, Grafana's
// trace-to-logs and Loki's derived fields have no identifier to join on.
func TestTelemetryInstallsTheTraceStampingHandler(t *testing.T) {
	keepDefaultLogger(t)

	cfg := offlineConfig(t, config.EntrypointAgent)
	governance, err := newPolicy(cfg, nil)
	if err != nil {
		t.Fatalf("newPolicy() error = %v, want nil", err)
	}

	var console bytes.Buffer
	build := buildinfo.Info{
		Mode: buildinfo.Development, Version: buildinfo.DevelopmentVersion,
		SourceIdentity: buildinfo.DevelopmentIdentity, Dirty: true,
	}
	flush, options, err := installTelemetry(t.Context(), &console, governance, build, launcherInstallsProviders)
	if err != nil {
		t.Fatalf("installTelemetry() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if flushErr := flush(t.Context()); flushErr != nil {
			t.Errorf("flush() error = %v, want nil", flushErr)
		}
	})
	if _, ok := slog.Default().Handler().(*telemetry.OtelHandler); !ok {
		t.Errorf("slog.Default() handler = %T, want the trace-stamping handler", slog.Default().Handler())
	}
	// ADK's launcher builds the tracer and logger providers itself; the runtime
	// only tells it what to attribute them to.
	if len(options) != 1 {
		t.Errorf("installTelemetry() returned %d launcher options, want exactly the resource", len(options))
	}

	slog.Default().Info("a console line")
	if !strings.Contains(console.String(), "a console line") {
		t.Errorf("console = %q, want the sanitized record", console.String())
	}
	log.Print("provider failed with password=super-secret-value-123456")
	if strings.Contains(console.String(), "super-secret-value-123456") {
		t.Errorf("standard library log retained a synthetic credential: %q", console.String())
	}

	// A process that installs its own providers has nothing to hand a launcher
	// it never runs.
	standaloneFlush, standaloneOptions, err := installTelemetry(
		t.Context(), &console, governance, build, processInstallsProviders,
	)
	if err != nil {
		t.Fatalf("installTelemetry() error = %v, want nil", err)
	}
	t.Cleanup(func() {
		if flushErr := standaloneFlush(t.Context()); flushErr != nil {
			t.Errorf("flush() error = %v, want nil", flushErr)
		}
	})
	if len(standaloneOptions) != 0 {
		t.Errorf("installTelemetry() returned %d launcher options, want none", len(standaloneOptions))
	}
}

// TestTelemetryOwnershipPathsEnforceADKTracePrivacy protects direct runtime
// assembly as well as the top-level command. The launcher-owned and standalone
// provider paths must both apply the same fail-closed sampler before any ADK
// provider can be constructed.
func TestTelemetryOwnershipPathsEnforceADKTracePrivacy(t *testing.T) {
	for name, ownsProviders := range map[string]bool{
		"launcher owns providers": launcherInstallsProviders,
		"process owns providers":  processInstallsProviders,
	} {
		t.Run(name, func(t *testing.T) {
			keepDefaultLogger(t)
			for _, variable := range []string{
				telemetry.EnvOTLPEndpoint,
				telemetry.EnvOTLPTracesEndpoint,
				telemetry.EnvOTLPMetricsEndpoint,
				telemetry.EnvOTLPLogsEndpoint,
			} {
				t.Setenv(variable, "")
			}
			t.Setenv(telemetry.EnvADKCaptureMessageContent, "false")
			t.Setenv("OTEL_TRACES_SAMPLER", "always_on")

			cfg := offlineConfig(t, config.EntrypointAgent)
			governance, err := newPolicy(cfg, nil)
			if err != nil {
				t.Fatalf("newPolicy() error = %v, want nil", err)
			}
			build := buildinfo.Info{
				Mode: buildinfo.Development, Version: buildinfo.DevelopmentVersion,
				SourceIdentity: buildinfo.DevelopmentIdentity, Dirty: true,
			}
			flush, _, err := installTelemetry(
				t.Context(), io.Discard, governance, build, ownsProviders,
			)
			if err != nil {
				t.Fatalf("installTelemetry() error = %v, want nil", err)
			}
			t.Cleanup(func() {
				if flushErr := flush(t.Context()); flushErr != nil {
					t.Errorf("flush() error = %v, want nil", flushErr)
				}
			})
			if got := os.Getenv("OTEL_TRACES_SAMPLER"); got != "always_off" {
				t.Errorf("OTEL_TRACES_SAMPLER = %q, want %q", got, "always_off")
			}
		})
	}
}

// TestSessionsSurviveARestart is the persistence guarantee: sessions live in a
// SQLite file inside AGENT_STATE_DIR, opened through the pure-Go dialector, so
// the binary keeps one SQLite implementation and no cgo.
func TestSessionsSurviveARestart(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
	recovered := recoveredTestState(t, cfg)

	first, err := newSessionService(cfg, recovered)
	if err != nil {
		t.Fatalf("newSessionService() error = %v, want nil", err)
	}
	created, err := first.Create(t.Context(), &session.CreateRequest{
		AppName: "agentops-agent",
		UserID:  "engineer",
		State:   map[string]any{"note": "before the restart"},
	})
	if err != nil {
		t.Fatalf("Create() error = %v, want nil", err)
	}

	// A second service over the same directory is what a restart looks like.
	if closeErr := first.Close(); closeErr != nil {
		t.Fatalf("closing the first process's session store: %v", closeErr)
	}
	second, err := newSessionService(cfg, recovered)
	if err != nil {
		t.Fatalf("newSessionService() after a restart error = %v, want nil", err)
	}
	got, err := second.Get(t.Context(), &session.GetRequest{
		AppName:   "agentops-agent",
		UserID:    "engineer",
		SessionID: created.Session.ID(),
	})
	if err != nil {
		t.Fatalf("Get() error = %v, want nil", err)
	}
	if got.Session.ID() != created.Session.ID() {
		t.Errorf("Get() = %q, want the session created before the restart", got.Session.ID())
	}
	value, err := got.Session.State().Get("note")
	if err != nil || value != "before the restart" {
		t.Errorf("State()[note] = %v, %v; want the value written before the restart", value, err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("closing the restarted session store: %v", err)
	}
	if _, err := newSessionService(config.Config{StateDir: string([]byte{0})}, recoveredState{}); err == nil {
		t.Error("newSessionService() error = nil for an unusable state directory")
	}
}

// TestSessionStoreOwnsARealCloseablePool prevents a lifecycle promise from
// depending on ADK's concrete database service. ADK v2.3.0 exposes no Close;
// the repository wrapper must therefore own and close the sql.DB it supplied.
func TestSessionStoreOwnsARealCloseablePool(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
	store, err := newSessionService(cfg, recoveredTestState(t, cfg))
	if err != nil {
		t.Fatalf("newSessionService() error = %v, want nil", err)
	}
	closer, ok := any(store).(io.Closer)
	if !ok {
		t.Fatalf("session store type = %T, want a repository-owned io.Closer", store)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("second Close() error = %v, want idempotent nil", err)
	}
	if _, err := store.Create(t.Context(), &session.CreateRequest{
		AppName: "agentops-agent", UserID: "engineer",
	}); err == nil {
		t.Error("Create() error = nil after the owned database pool was closed")
	}
}

// TestTheSessionStoreIsOpenedUnmigratedForA2A covers the course invariant the
// A2A startup sequence rests on.
//
// Creating ADK's schema is a startup step the A2A server runs *after* an
// interrupted state restore has been recovered. Opening the store must
// therefore not migrate it: until recovery has run, a half-published generation
// is indistinguishable from a healthy one, and a migration would write into it.
func TestTheSessionStoreIsOpenedUnmigratedForA2A(t *testing.T) {
	t.Parallel()

	cfg := config.Config{StateDir: filepath.Join(t.TempDir(), "state")}
	sessions, err := openSessionStore(cfg, recoveredTestState(t, cfg))
	if err != nil {
		t.Fatalf("openSessionStore() error = %v, want nil", err)
	}
	if _, err := sessions.Create(t.Context(), &session.CreateRequest{
		AppName: "agentops-agent", UserID: "engineer",
	}); err == nil {
		t.Error("Create() error = nil on an unmigrated store; the schema was created too early")
	}
	if err := sessions.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
}

// TestGuardHonoursTheCircuitBreakerToggle pins the shipped default: the breaker
// registry exists only when AGENT_CIRCUIT_BREAKER_ENABLED is on, so the default
// behavior is retry-only and no breaker is created for any tool.
func TestGuardHonoursTheCircuitBreakerToggle(t *testing.T) {
	t.Parallel()

	base := config.Config{
		ToolTimeout:             config.Seconds(30),
		RetryBackoff:            config.Seconds(0.5),
		MaxRetries:              2,
		CircuitFailureThreshold: 5,
		CircuitResetTimeout:     config.Seconds(30),
	}

	off, err := newGuard(base)
	if err != nil {
		t.Fatalf("newGuard() error = %v, want nil", err)
	}
	if off.Breakers() != nil {
		t.Error("a breaker registry exists with the toggle off")
	}
	if off.Attempts() != base.MaxRetries+1 {
		t.Errorf("Attempts() = %d, want %d", off.Attempts(), base.MaxRetries+1)
	}

	enabled := base
	enabled.CircuitBreakerEnabled = true
	on, err := newGuard(enabled)
	if err != nil {
		t.Fatalf("newGuard() error = %v, want nil", err)
	}
	if on.Breakers() == nil {
		t.Error("no breaker registry with the toggle on")
	}

	// A deadline of zero would silently disable a Chapter 4.5 guarantee, so it
	// is refused rather than defaulted.
	if _, err := newGuard(config.Config{}); err == nil {
		t.Error("newGuard() error = nil for a zero tool timeout")
	}
}

// TestActionableErrorClassifiesOnlyFirstPartyFailures covers the error-hygiene
// extractor: only a summary built from a matched first-party type reaches the
// model; untrusted outer wrappers and retained causes never do.
func TestActionableErrorClassifiesOnlyFirstPartyFailures(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		err       error
		forbidden string
		want      bool
	}{
		"circuit open": {err: &resilience.CircuitOpenError{}, want: true},
		"deadline":     {err: &resilience.DeadlineError{}, want: true},
		"retries exhausted": {
			err: &resilience.RetriesExhaustedError{
				Tool: "get_incident", Attempts: 3,
				Err: errors.New("password=SYNTHETIC_DO_NOT_USE_RETRY_CAUSE_123456"),
			},
			forbidden: "SYNTHETIC_DO_NOT_USE",
			want:      true,
		},
		"data access": {err: data.ErrDataAccess, want: false},
		"wrapped": {
			err: fmt.Errorf("untrusted outer body: %w", &resilience.DeadlineError{
				Tool: "get_incident", Timeout: time.Second,
			}),
			forbidden: "untrusted outer body",
			want:      true,
		},
		"anything else": {err: errors.New("pq: relation \"secrets\" does not exist"), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			summary, got := actionableError(testCase.err)
			if got != testCase.want {
				t.Errorf("actionableError(%v) = (_, %v), want (_, %v)", testCase.err, got, testCase.want)
			}
			if testCase.forbidden != "" && strings.Contains(summary, testCase.forbidden) {
				t.Errorf("actionableError() summary = %q, retained %q", summary, testCase.forbidden)
			}
		})
	}
}

// TestMCPRouteIsOffUntilItsURLIsSet pins the account-free default: with no URL
// the six reads run in process, under the local deadline, retries and circuit
// breaker.
func TestMCPRouteIsOffUntilItsURLIsSet(t *testing.T) {
	t.Parallel()

	toolset, err := newMCPToolset(config.Config{})
	if err != nil {
		t.Fatalf("newMCPToolset() error = %v, want nil", err)
	}
	if toolset != nil {
		t.Error("an MCP route exists with no URL configured")
	}

	routed, err := newMCPToolset(config.Config{
		MCPURL:      "http://127.0.0.1:3000/mcp",
		ToolTimeout: config.Seconds(30),
	})
	if err != nil {
		t.Fatalf("newMCPToolset() error = %v, want nil", err)
	}
	if routed == nil {
		t.Error("no MCP route with a URL configured")
	}

	// Without a deadline the route is refused rather than built: a hung gateway
	// must fail a tool call fast, not hang the turn.
	if _, err := newMCPToolset(config.Config{MCPURL: "http://127.0.0.1:3000/mcp"}); !errors.Is(err, compose.ErrMCP) {
		t.Errorf("newMCPToolset() error = %v, want ErrMCP", err)
	}
}

// TestBothAgentsArePublished covers the loader.
//
// The structured report is a second, independently addressable agent rather than
// a mode of the root one, which is what lets an evaluation drive the schema path
// over the REST surface without changing AGENT_ENTRYPOINT. The console and the
// A2A card still serve the root agent alone, so RootAgent must stay the
// entrypoint's choice.
func TestBothAgentsArePublished(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		entrypoint config.Entrypoint
		wantRoot   string
	}{
		{config.EntrypointAgent, compose.AgentName},
		{config.EntrypointWorkflow, compose.WorkflowName},
		{config.EntrypointCoordinator, compose.CoordinatorName},
	} {
		t.Run(string(testCase.entrypoint), func(t *testing.T) {
			t.Parallel()

			loader, err := newAgentLoader(wholeComposition(t, testCase.entrypoint))
			if err != nil {
				t.Fatalf("newAgentLoader() error = %v, want nil", err)
			}
			if got := loader.RootAgent().Name(); got != testCase.wantRoot {
				t.Errorf("RootAgent() = %q, want %q", got, testCase.wantRoot)
			}
			published := slices.Sorted(slices.Values(loader.ListAgents()))
			want := slices.Sorted(slices.Values([]string{testCase.wantRoot, compose.ReportAgentName}))
			if !reflect.DeepEqual(published, want) {
				t.Errorf("ListAgents() = %v, want %v", published, want)
			}
			report, err := loader.LoadAgent(compose.ReportAgentName)
			if err != nil {
				t.Fatalf("LoadAgent(%q) error = %v, want nil", compose.ReportAgentName, err)
			}
			if report.Name() != compose.ReportAgentName {
				t.Errorf("LoadAgent() = %q, want %q", report.Name(), compose.ReportAgentName)
			}
			if _, err := loader.LoadAgent("anything-else"); err == nil {
				t.Error("LoadAgent() error = nil for an agent this binary never published")
			}
		})
	}
}

// TestPolicyTrustsOnlyTheLocalLoadSkillTool covers the governance wiring.
//
// The carve-out is granted to the exact tool value the locally built toolset
// produced. Keying it on a name would hand the boundary to whichever MCP server
// happened to advertise that name.
func TestPolicyTrustsOnlyTheLocalLoadSkillTool(t *testing.T) {
	t.Parallel()

	skills, err := compose.NewSkills(t.Context(), compose.SkillsDir(repositoryDataset))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	trusted := []tool.Tool{skills.LoadSkillTool()}
	governance, err := newPolicy(config.Config{SanitizeToolOutput: true}, trusted)
	if err != nil {
		t.Fatalf("newPolicy() error = %v, want nil", err)
	}
	if governance == nil {
		t.Fatal("newPolicy() = nil")
	}
	if _, err := governance.Plugin(); err != nil {
		t.Errorf("Plugin() error = %v, want nil", err)
	}
}

// TestTheFailureReportNamesTheLauncherSyntaxOnlyForTheLauncher pins where an
// operator is sent after a startup failure.
//
// An invalid provider is the cheapest offline way to make configuration refuse
// before any model or filesystem boundary is opened.
func TestTheFailureReportNamesTheLauncherSyntaxOnlyForTheLauncher(t *testing.T) {
	isolatedEnvironment(t)
	const invalidProvider = "unsupported-provider"
	t.Setenv(config.EnvModelProvider, invalidProvider)

	var out, errOut bytes.Buffer
	if code := execute(t.Context(), nil, &out, &errOut); code != config.ExitInvalid {
		t.Fatalf("execute() = %d, want %d; stderr: %s", code, config.ExitInvalid, errOut.String())
	}
	if !strings.Contains(errOut.String(), invalidProvider) {
		t.Errorf("stderr = %q, want it to quote the provider that refused", errOut.String())
	}
	// An operator who sees a startup failure needs to know how to reach the
	// other modes, config:check among them.
	if !strings.Contains(errOut.String(), "console") {
		t.Error("the failure report omits the launcher syntax")
	}
}

// offlineToolSurface builds the eight function tools over the committed dataset
// and a disposable state directory.
//
// Nothing here touches the filesystem: the tools are constructed, not called,
// and the memory plane opens its databases lazily on first use.
func offlineToolSurface(t *testing.T, entrypoint config.Entrypoint) (compose.Tools, *memory.Memory) {
	t.Helper()

	cfg := offlineConfig(t, entrypoint)
	store := data.New(data.Config{DataDir: cfg.DataDir, StateDir: cfg.StateDir})
	surface, memories, err := newToolSurface(cfg, store, passThroughGuard, func(text string) string { return text })
	if err != nil {
		t.Fatalf("newToolSurface() error = %v, want nil", err)
	}
	return surface, memories
}

// wholeComposition builds a composer with every tool the compositions require,
// over a model that answers nothing: the loader never calls it.
func wholeComposition(t *testing.T, entrypoint config.Entrypoint) *compose.Compose {
	t.Helper()

	surface, memories := offlineToolSurface(t, entrypoint)
	skills, err := compose.NewSkills(t.Context(), compose.SkillsDir(repositoryDataset))
	if err != nil {
		t.Fatalf("NewSkills() error = %v, want nil", err)
	}
	composer, err := compose.New(compose.Config{
		Model:      silentLLM{},
		Skills:     skills,
		Tools:      surface,
		Memory:     memories.MemoryTools(),
		Entrypoint: entrypoint,
	})
	if err != nil {
		t.Fatalf("compose.New() error = %v, want nil", err)
	}
	return composer
}

// silentLLM is a model that answers nothing; the loader never calls it.
type silentLLM struct{}

func (silentLLM) Name() string { return "silent" }

func (silentLLM) GenerateContent(
	context.Context, *adkmodel.LLMRequest, bool,
) iter.Seq2[*adkmodel.LLMResponse, error] {
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		yield(nil, errors.New("silentLLM must not be called: the wiring tests are offline"))
	}
}
