package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/MLOps-Courses/agentops-open-course/evals"
)

// commands is the subcommand list, in the order a learner meets them, with the one
// line each contributes to `agentops-eval help`. The same line prints above that
// subcommand's own flags, so the two views cannot drift apart.
var commands = []struct{ name, purpose string }{
	{"validate", "check every committed evalset, domain reference, dashboard, and the import boundary"},
	{"run", "run a model-backed evaluation and write a sanitized run artifact"},
	{"calibrate", "measure the configured judge against the labeled calibration set"},
	{"compare", "compare two run artifacts and fail on a deterministic regression"},
	{"retrieval", "compare keyword and semantic runbook retrieval over an isolated MCP runtime"},
}

func usage() string {
	lines := []string{"agentops-eval validates assets and runs black-box agent evaluations.", "", "Usage:"}
	for _, command := range commands {
		lines = append(lines, fmt.Sprintf("  agentops-eval %-10s[flags]  %s", command.name, command.purpose))
	}
	return strings.Join(append(lines, "", "Run `agentops-eval <command> --help` for that command's flags.", ""), "\n")
}

func purposeOf(name string) string {
	for _, command := range commands {
		if command.name == name {
			return command.purpose
		}
	}
	return ""
}

// parseFlags parses one subcommand's arguments and answers `--help` itself.
//
// Every subcommand discards the flag package's own output so that a bad flag is
// reported once, by main, rather than printed twice. That also swallowed `--help`:
// Parse returns flag.ErrHelp having written nothing, and the learner saw the bare
// line "flag: help requested" and exit code 1 from a command this course tells them
// to run. Printing the defaults here and reporting the request as handled fixes that
// without turning a flag package into a CLI framework.
func parseFlags(flags *flag.FlagSet, arguments []string, stdout io.Writer) (bool, error) {
	flags.SetOutput(io.Discard)
	err := flags.Parse(arguments)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, flag.ErrHelp) {
		return false, err
	}
	if _, writeErr := fmt.Fprintf(
		stdout, "agentops-eval %s: %s\n\nFlags:\n", flags.Name(), purposeOf(flags.Name()),
	); writeErr != nil {
		return true, writeErr
	}
	flags.SetOutput(stdout)
	flags.PrintDefaults()
	return true, nil
}

func main() {
	if err := execute(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agentops-eval:", err)
		os.Exit(1)
	}
}

func execute(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("command is required; use --help")
	}
	switch arguments[0] {
	case "validate":
		return validateCommand(ctx, arguments[1:], stdout)
	case "run":
		return runCommand(ctx, arguments[1:], stdout, stderr)
	case "calibrate":
		return calibrateCommand(ctx, arguments[1:], stdout)
	case "compare":
		return compareCommand(arguments[1:], stdout)
	case "retrieval":
		return retrievalCommand(ctx, arguments[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, err := io.WriteString(stdout, usage())
		return err
	default:
		return fmt.Errorf("unknown command %q\n%s", arguments[0], usage())
	}
}

func retrievalCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("retrieval", flag.ContinueOnError)
	moduleDir := flags.String("eval-dir", detectEvalDir(), "evaluation module directory")
	dataDir := flags.String("data-dir", "", "immutable agent data directory")
	output := flags.String("output", "retrieval-results.json", "sanitized retrieval artifact")
	binary := flags.String("agent-binary", "", "compiled Go agent binary")
	embeddingModel := flags.String(
		"embedding-model", getenv("AGENT_EMBEDDING_MODEL", "nomic-embed-text"), "embedding model evidence",
	)
	embeddingModelDigest := flags.String(
		"embedding-model-digest", os.Getenv("EVAL_EMBEDDING_MODEL_DIGEST"), "optional immutable embedding model digest",
	)
	requestTimeout := flags.Duration("request-timeout", 5*time.Minute, "one retrieval-tool request deadline")
	if handled, err := parseFlags(flags, arguments, stdout); err != nil || handled {
		return err
	}
	if *requestTimeout < 0 {
		return errors.New("retrieval timeouts must not be negative")
	}
	source, err := resolveCheckout(ctx)
	if err != nil {
		return err
	}
	paths := assetPaths(*moduleDir, *dataDir)
	if *binary == "" {
		*binary = filepath.Join(*moduleDir, "..", "agents", "go", "bin", "agent")
	}
	artifact, err := evals.RunMCPRetrievalEvaluation(ctx, evals.MCPRetrievalEvaluationConfig{
		Source: source, EmbeddingModelDigest: *embeddingModelDigest,
		Runtime: evals.MCPRetrievalRuntimeFactoryConfig{
			Binary: *binary, DataDir: paths.DataDir,
			EmbeddingModel: *embeddingModel,
			Environment:    agentRuntimeEnvironment(), Timeout: *requestTimeout, Output: stderr,
		},
	})
	if err != nil {
		return err
	}
	if err := evals.WriteRetrievalArtifact(resolveFrom(*moduleDir, *output), artifact); err != nil {
		return err
	}
	return writeJSON(stdout, artifact)
}

func compareCommand(arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	moduleDir := flags.String("eval-dir", detectEvalDir(), "evaluation module directory")
	baselinePath := flags.String("baseline", "", "reviewed baseline artifact")
	candidatePath := flags.String("candidate", "", "candidate artifact")
	output := flags.String("output", "prompt-comparison.json", "sanitized comparison artifact")
	requireDistinctSource := flags.Bool("require-distinct-source", false, "require artifacts from different Git revisions")
	if handled, err := parseFlags(flags, arguments, stdout); err != nil || handled {
		return err
	}
	if *baselinePath == "" || *candidatePath == "" {
		return errors.New("compare requires explicit --baseline and --candidate artifacts")
	}
	baseline, err := evals.LoadRunArtifact(resolveFrom(*moduleDir, *baselinePath))
	if err != nil {
		return err
	}
	candidate, err := evals.LoadRunArtifact(resolveFrom(*moduleDir, *candidatePath))
	if err != nil {
		return err
	}
	var comparison evals.ComparisonArtifact
	if *requireDistinctSource {
		comparison, err = evals.ComparePromptRuns(baseline, candidate)
	} else {
		comparison, err = evals.CompareRuns(baseline, candidate)
	}
	if err != nil {
		return err
	}
	if err := evals.WriteJSONArtifact(resolveFrom(*moduleDir, *output), comparison); err != nil {
		return err
	}
	if err := writeJSON(stdout, comparison); err != nil {
		return err
	}
	if !comparison.DeterministicPass {
		return errors.New("candidate regressed a deterministic score or its deterministic pass rate")
	}
	return nil
}

func validateCommand(ctx context.Context, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	moduleDir := flags.String("eval-dir", detectEvalDir(), "evaluation module directory")
	dataDir := flags.String("data-dir", "", "immutable agent data directory")
	if handled, err := parseFlags(flags, arguments, stdout); err != nil || handled {
		return err
	}
	paths := assetPaths(*moduleDir, *dataDir)
	summary, err := evals.ValidateAssets(ctx, paths)
	if err != nil {
		return err
	}
	return writeJSON(stdout, summary)
}

func runCommand(ctx context.Context, arguments []string, stdout, stderr io.Writer) (returnErr error) {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	moduleDir := flags.String("eval-dir", detectEvalDir(), "evaluation module directory")
	dataDir := flags.String("data-dir", "", "immutable agent data directory")
	evalsetPath := flags.String("evalset", "ops.evalset.json", "evalset path relative to eval-dir")
	output := flags.String("output", "results.json", "sanitized run artifact path relative to eval-dir")
	agentRuntime := flags.String("agent-runtime", getenv("EVAL_AGENT_RUNTIME", "process"), "agent runtime: process or container")
	binary := flags.String("agent-binary", "", "compiled Go agent binary")
	image := flags.String("agent-image", os.Getenv("EVAL_AGENT_IMAGE"), "containerized Go agent image")
	containerEngine := flags.String("container-engine", getenv("EVAL_CONTAINER_ENGINE", "docker"), "OCI container engine")
	transport := flags.String("transport", "rest", "rest or a2a")
	entrypoint := flags.String("entrypoint", "agent", "agent, workflow, or coordinator")
	appName := flags.String("app-name", "", "REST app override")
	streaming := flags.Bool("stream", false, "use ADK REST SSE")
	repeat := flags.Int("repeat", 1, "samples per case")
	minimumPassRate := flags.Float64("min-pass-rate", 1, "minimum run pass rate")
	provider := flags.String("model-provider", getenv("AGENT_MODEL_PROVIDER", "openai-compatible"), "model provider evidence")
	model := flags.String("model", getenv("AGENT_MODEL", "qwen3:4b-instruct"), "model evidence")
	modelDigest := flags.String("model-digest", os.Getenv("EVAL_MODEL_DIGEST"), "optional immutable model digest evidence")
	runID := flags.String("run-id", "", "evaluation run id")
	requireGrounded := flags.Bool("require-grounded", false, "enable entity groundedness scoring")
	requireSchema := flags.Bool("require-schema", false, "enable strict triage-report scoring")
	withJudge := flags.Bool("judge", false, "enable calibrated gateway judge scoring")
	otelEndpoint := flags.String("otel-endpoint", os.Getenv("EVAL_OTEL_EXPORTER_OTLP_ENDPOINT"), "evaluation-only OTLP HTTP endpoint")
	// One replaceable flag rather than a repeatable one. `mise run eval -- ...` appends
	// to the task's own arguments, so a repeatable flag can only ever grow: pointing the
	// run at another evalset would carry the operations safety cases into it and fail
	// validation. A single comma-separated value lets the last one win, which is what
	// makes every flag recipe in README.md a real override of the taught command.
	requiredCases := flags.String("required-cases", "", "comma-separated case ids that must pass in every sample; empty clears them")
	if handled, err := parseFlags(flags, arguments, stdout); err != nil || handled {
		return err
	}
	source, err := resolveCheckout(ctx)
	if err != nil {
		return err
	}
	if *runID == "" {
		resolved, runIDErr := evals.NewRunID()
		if runIDErr != nil {
			return runIDErr
		}
		*runID = resolved
	}
	paths := assetPaths(*moduleDir, *dataDir)
	evalset, err := evals.LoadEvalSet(resolveFrom(*moduleDir, *evalsetPath))
	if err != nil {
		return err
	}
	domain, err := evals.LoadDomain(paths.DataDir)
	if err != nil {
		return err
	}
	if domainErr := evalset.ValidateDomain(domain); domainErr != nil {
		return domainErr
	}
	if *agentRuntime == "process" && *binary == "" {
		*binary = filepath.Join(*moduleDir, "..", "agents", "go", "bin", "agent")
	}
	environment := agentRuntimeEnvironment()
	turnTimeout, err := secondsFromEnvironment("EVAL_TURN_TIMEOUT_S")
	if err != nil {
		return err
	}
	factory, err := selectClientFactory(clientFactoryConfig{
		Runtime: *agentRuntime,
		Process: evals.ProcessClientFactoryConfig{
			Source: source, Binary: *binary, DataDir: paths.DataDir, Transport: *transport,
			Entrypoint: *entrypoint, AppName: *appName, Streaming: *streaming,
			Environment: environment, Output: stderr, Timeout: turnTimeout,
		},
		Container: evals.ContainerClientFactoryConfig{
			Source: source, Engine: *containerEngine, Image: *image,
			Transport: *transport, Entrypoint: *entrypoint,
			AppName: *appName, Streaming: *streaming, Environment: environment,
			Output: stderr, Timeout: turnTimeout,
		},
	}, defaultClientFactoryConstructors(ctx))
	if err != nil {
		return err
	}
	recorder, err := recorder(ctx, *otelEndpoint)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, recorder.Shutdown(ctx)) }()
	config := evals.RunnerConfig{
		EvalSet: evalset, Domain: domain, RunID: *runID, Source: source,
		Model:     evals.ModelEvidence{Provider: *provider, Name: *model, Digest: *modelDigest},
		Transport: *transport, Repeat: *repeat, MinimumPassRate: *minimumPassRate,
		RequiredCases: splitCaseList(*requiredCases), RequireGrounded: *requireGrounded,
		RequireSchema: *requireSchema,
		Recorder:      recorder, ClientFactory: factory,
	}
	if *withJudge {
		judgeClient, judgeErr := gatewayJudgeFromEnvironment()
		if judgeErr != nil {
			return judgeErr
		}
		config.Judge = judgeClient
	}
	outputPath := resolveFrom(*moduleDir, *output)
	// Read the previous results before the run overwrites them: the only cost
	// baseline this harness keeps is "what the last run of this evalset spent".
	previous, hasPrevious := previousRunArtifact(outputPath)
	artifact, err := evals.Run(ctx, config)
	if err != nil {
		// Defense in depth for a run that costs hours: whatever was graded before the
		// failure is written out so a fatal path never discards all of it. It goes to
		// its own path, because an exit code does not survive into an uploaded CI
		// artifact and the file has to carry the distinction on its own.
		if len(artifact.Cases) > 0 {
			partialPath, writeErr := writePartialArtifact(outputPath, artifact)
			if writeErr != nil {
				return errors.Join(err, writeErr)
			}
			// Named on stderr because nothing else points at it: the release path is
			// untouched, so a learner would otherwise have no reason to look for it.
			if _, printErr := fmt.Fprintln(stderr, "agentops-eval: partial evidence written to", partialPath); printErr != nil {
				return errors.Join(err, printErr)
			}
		}
		return err
	}
	if hasPrevious {
		warning, driftErr := evals.TokenDriftWarning(previous, artifact)
		if driftErr != nil {
			return driftErr
		}
		if warning != "" {
			if _, printErr := fmt.Fprintln(stderr, "agentops-eval:", warning); printErr != nil {
				return printErr
			}
		}
	}
	if err := evals.WriteJSONArtifact(outputPath, artifact); err != nil {
		return err
	}
	if err := writeJSON(stdout, artifact.Summary); err != nil {
		return err
	}
	if !artifact.Passed() {
		return errors.New("evaluation did not clear its pass and required-case thresholds")
	}
	return nil
}

// writePartialArtifact publishes the case samples a failed run had already graded,
// beside the release artifact rather than over it, and returns where it landed.
//
// Two separate reasons keep it off the release path. It must not clobber the last
// complete run, which is the only cost baseline and often the only evidence anyone
// still has; and `results.json` is the name CI uploads and a human reads, so a
// truncated file under that name is read as the run. The prefix rather than a suffix
// is what puts it inside the repository's existing `evals/*-results.json` ignore
// rule: a failed run must not leave a learner an untracked file to explain.
//
// The artifact refuses the claim as well as the name — it carries the sample count
// the whole evalset was asked for, so RunArtifact.Passed() is false on it — because
// a reader who renames the file must still be told what it is.
func writePartialArtifact(outputPath string, artifact evals.RunArtifact) (string, error) {
	directory, name := filepath.Split(outputPath)
	path := filepath.Join(directory, "partial-"+name)
	return path, evals.WriteJSONArtifact(path, artifact)
}

// previousRunArtifact treats an unreadable or older results file as "no
// previous run". A cost comparison is a courtesy, never a reason to fail.
func previousRunArtifact(path string) (evals.RunArtifact, bool) {
	artifact, err := evals.LoadRunArtifact(path)
	if err != nil {
		return evals.RunArtifact{}, false
	}
	return artifact, true
}

func calibrateCommand(ctx context.Context, arguments []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("calibrate", flag.ContinueOnError)
	moduleDir := flags.String("eval-dir", detectEvalDir(), "evaluation module directory")
	calibrationPath := flags.String("calibration", "judge-calibration.json", "labeled calibration set")
	output := flags.String("output", "judge-calibration-results.json", "sanitized calibration artifact")
	if handled, err := parseFlags(flags, arguments, stdout); err != nil || handled {
		return err
	}
	source, err := resolveCheckout(ctx)
	if err != nil {
		return err
	}
	set, err := evals.LoadCalibrationSet(resolveFrom(*moduleDir, *calibrationPath))
	if err != nil {
		return err
	}
	judge, err := gatewayJudgeFromEnvironment()
	if err != nil {
		return err
	}
	result, err := evals.Calibrate(ctx, set, judge, evals.ModelEvidence{
		Provider: getenv("EVAL_JUDGE_PROVIDER", os.Getenv("AGENT_MODEL_PROVIDER")),
		Name:     os.Getenv("EVAL_JUDGE_MODEL"),
		Digest:   getenv("EVAL_JUDGE_MODEL_DIGEST", os.Getenv("EVAL_MODEL_DIGEST")),
	}, source)
	if err != nil {
		return err
	}
	if err := evals.WriteJSONArtifact(resolveFrom(*moduleDir, *output), result); err != nil {
		return err
	}
	// Calibration reports, it does not judge the judge. A low agreement number
	// is a reason to read the per-case matches, not an automatic build failure.
	if _, err := fmt.Fprintf(
		stdout, "judge agreement: %.3f (%d of %d labeled cases)\n",
		result.Agreement, result.Matches, result.Total,
	); err != nil {
		return err
	}
	// The baseline is what an always-same-answer judge would score on this label
	// split, so agreement at or below it measures the split rather than the judge.
	if _, err := fmt.Fprintf(
		stdout, "majority baseline: %.3f (%d false pass, %d false fail)\n",
		result.MajorityBaseline, result.FalsePass, result.FalseFail,
	); err != nil {
		return err
	}
	return writeJSON(stdout, result)
}

func assetPaths(moduleDir, dataDir string) evals.AssetPaths {
	if dataDir == "" {
		dataDir = filepath.Join(moduleDir, "..", "agents", "data")
	}
	return evals.AssetPaths{
		ModuleDir: moduleDir, DataDir: dataDir,
		EvalSets: []string{
			filepath.Join(moduleDir, "ops.evalset.json"),
			filepath.Join(moduleDir, "workflow.evalset.json"),
			filepath.Join(moduleDir, "triage-report.evalset.json"),
		},
		Calibration: filepath.Join(moduleDir, "judge-calibration.json"),
		Dashboard:   filepath.Join(moduleDir, "grafana-dashboard.json"),
	}
}

func recorder(ctx context.Context, endpoint string) (evals.EvidenceRecorder, error) {
	if endpoint != "" {
		return evals.NewOTLPRecorder(ctx, endpoint)
	}
	return evals.NewNoopRecorder()
}

func gatewayJudgeFromEnvironment() (*evals.GatewayJudge, error) {
	timeout, err := judgeTimeoutFromEnvironment()
	if err != nil {
		return nil, err
	}
	return evals.NewGatewayJudge(evals.GatewayJudgeConfig{
		BaseURL: os.Getenv("EVAL_JUDGE_BASE_URL"),
		Model:   os.Getenv("EVAL_JUDGE_MODEL"),
		APIKey:  os.Getenv("EVAL_JUDGE_API_KEY"),
		Timeout: timeout,
	})
}

// secondsFromEnvironment reads one optional deadline, in whole seconds.
//
// Two deadlines bound a model-backed run and both were hard-coded before this:
// EVAL_TURN_TIMEOUT_S for one agent turn, EVAL_JUDGE_TIMEOUT_S for one judge
// call. Neither default is wrong — they are wrong *for a host*, and the host is
// the thing the harness cannot know. A CPU-only machine answering a five-tool
// trajectory can spend longer on one turn than the thirty minutes the client
// allowed, and the run then fails on a case that was working.
//
// Unset returns zero, which every caller reads as "keep the built-in default".
// Zero and negative are refused rather than treated as "no deadline": a turn that
// cannot time out cannot be told apart from one that hung.
func secondsFromEnvironment(name string) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive whole number of seconds, got %q", name, raw)
	}
	return time.Duration(seconds) * time.Second, nil
}

// judgeTimeoutFromEnvironment reads EVAL_JUDGE_TIMEOUT_S, in seconds.
//
// It exists for the same reason AGENT_MODEL_TIMEOUT_S does, and was added after
// the fixed five-minute default made `--judge` impossible to complete on the
// hardware this course calls the required path: a judge call carries the
// question, the answer, and the reference answer, so it is a larger prompt than
// the turn it is grading, and on a CPU-only host it can take longer than the
// turn did. A deadline nobody can raise is a deadline that decides which
// machines may run the evidence.
//
// Unset keeps the five-minute default. Zero and negative are refused rather than
// treated as "no deadline": an unbounded judge call cannot be told apart from a
// hung one.
func judgeTimeoutFromEnvironment() (time.Duration, error) {
	return secondsFromEnvironment("EVAL_JUDGE_TIMEOUT_S")
}

func detectEvalDir() string {
	if _, err := os.Stat("ops.evalset.json"); err == nil {
		return "."
	}
	return "evals"
}

func resolveFrom(directory, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(directory, path)
}

type sourceCommand func(context.Context, string, ...string) ([]byte, error)

// checkoutIdentity is the JSON that tools/cmd/source-identity prints. The
// agent build stamps the same three fields into the binary, which is what
// makes the runtime freshness check a plain comparison.
type checkoutIdentity struct {
	Revision   string `json:"revision"`
	TreeDigest string `json:"tree_digest"`
	Dirty      bool   `json:"dirty"`
}

func resolveCheckout(ctx context.Context) (evals.SourceEvidence, error) {
	return resolveCheckoutWithCommand(ctx, executeSourceCommand)
}

func resolveCheckoutWithCommand(ctx context.Context, run sourceCommand) (evals.SourceEvidence, error) {
	if run == nil {
		return evals.SourceEvidence{}, errors.New("source identity command is unavailable")
	}
	rootOutput, err := run(ctx, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return evals.SourceEvidence{}, fmt.Errorf("locate source checkout: %w", err)
	}
	root := strings.TrimSpace(string(rootOutput))
	if root == "" {
		return evals.SourceEvidence{}, errors.New("locate source checkout: git returned an empty root")
	}
	output, err := run(ctx, "go", "-C", filepath.Join(root, "tools"), "run", "./cmd/source-identity", "--root", root)
	if err != nil {
		return evals.SourceEvidence{}, fmt.Errorf("resolve source identity: %w", err)
	}
	var identity checkoutIdentity
	if err := json.Unmarshal(output, &identity); err != nil {
		return evals.SourceEvidence{}, fmt.Errorf("decode source identity: %w", err)
	}
	evidence := evals.SourceEvidence{
		Revision: identity.Revision, TreeDigest: identity.TreeDigest, Dirty: identity.Dirty,
	}
	if err := evidence.Validate(); err != nil {
		return evals.SourceEvidence{}, fmt.Errorf("validate source identity: %w", err)
	}
	return evidence, nil
}

func executeSourceCommand(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return nil, fmt.Errorf("%w: %s", err, message)
		}
		return nil, err
	}
	return output, nil
}

type clientFactoryConfig struct {
	Runtime   string
	Process   evals.ProcessClientFactoryConfig
	Container evals.ContainerClientFactoryConfig
}

type clientFactoryConstructors struct {
	process   func(evals.ProcessClientFactoryConfig) (evals.ClientFactory, error)
	container func(evals.ContainerClientFactoryConfig) (evals.ClientFactory, error)
}

func defaultClientFactoryConstructors(ctx context.Context) clientFactoryConstructors {
	return clientFactoryConstructors{
		process: func(config evals.ProcessClientFactoryConfig) (evals.ClientFactory, error) {
			return evals.NewProcessClientFactory(ctx, config)
		},
		container: func(config evals.ContainerClientFactoryConfig) (evals.ClientFactory, error) {
			return evals.NewContainerClientFactory(ctx, config)
		},
	}
}

func selectClientFactory(
	config clientFactoryConfig, constructors clientFactoryConstructors,
) (evals.ClientFactory, error) {
	switch config.Runtime {
	case "process":
		if constructors.process == nil {
			return nil, errors.New("process runtime constructor is unavailable")
		}
		return constructors.process(config.Process)
	case "container":
		if constructors.container == nil {
			return nil, errors.New("container runtime constructor is unavailable")
		}
		return constructors.container(config.Container)
	default:
		return nil, fmt.Errorf("unsupported agent runtime %q; use process or container", config.Runtime)
	}
}

func agentRuntimeEnvironment() map[string]string {
	environment := make(map[string]string)
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "AGENT_") {
			environment[key] = value
			continue
		}
		switch key {
		case "OPENAI_BASE_URL", "OPENAI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_ENTERPRISE",
			"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION":
			environment[key] = value
		}
	}
	return environment
}

func getenv(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// splitCaseList parses a comma-separated case list, ignoring blanks so that an empty
// value means "no required cases" rather than one case with an empty id.
func splitCaseList(value string) []string {
	var cases []string
	for _, item := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			cases = append(cases, trimmed)
		}
	}
	return cases
}
