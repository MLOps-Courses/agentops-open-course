package compose

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
	"github.com/MLOps-Courses/agentops-open-course/agents/go/tools"
)

// validReport is the payload the Python suite uses, built from the vocabulary
// rather than from literals so the domain portability ratchet stays shut and
// the test survives a pivoted domain.
func validReport(t *testing.T) domain.TriageReportPayload {
	t.Helper()

	vocabulary := domain.Reference()
	return domain.TriageReportPayload{
		IncidentID:         vocabulary.Incidents.InventoryDown,
		Severity:           string(domain.SeveritySev1),
		AffectedServices:   []string{vocabulary.Services.Inventory},
		Hypothesis:         "Pods crash-loop after the bad deploy exhausted memory.",
		Evidence:           []string{"pods restart every 30s", "HTTP 503 on stock lookups"},
		RecommendedRunbook: vocabulary.Runbooks.ServiceDown,
		ProposedActions:    []string{tools.RestartServiceToolName + " (needs approval)"},
	}
}

// encode renders a payload the way a model would answer.
func encode(t *testing.T, payload any) string {
	t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v, want nil", err)
	}
	return string(encoded)
}

// TestValidReportParses is the Go port of test_valid_report_parses.
func TestValidReportParses(t *testing.T) {
	t.Parallel()

	payload := validReport(t)
	parsed, err := ParseTriageReport(encode(t, payload))
	if err != nil {
		t.Fatalf("ParseTriageReport() error = %v, want nil", err)
	}
	if string(parsed.IncidentID()) != payload.IncidentID {
		t.Errorf("IncidentID() = %q, want %q", parsed.IncidentID(), payload.IncidentID)
	}
}

// TestFencedReportParses is the Go port of test_markdown_fenced_report_parses.
//
// A leading fence is the one quirk local models add anyway, so it is tolerated;
// everything else about the answer still has to validate.
func TestFencedReportParses(t *testing.T) {
	t.Parallel()

	payload := validReport(t)
	body := encode(t, payload)
	for name, answer := range map[string]string{
		"tagged":     "```json\n" + body + "\n```",
		"untagged":   "```\n" + body + "\n```",
		"unclosed":   "```json\n" + body,
		"surrounded": "  ```json\n" + body + "\n```  ",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			parsed, err := ParseTriageReport(answer)
			if err != nil {
				t.Fatalf("ParseTriageReport() error = %v, want nil", err)
			}
			if string(parsed.RecommendedRunbook()) != payload.RecommendedRunbook {
				t.Errorf("RecommendedRunbook() = %q, want %q", parsed.RecommendedRunbook(), payload.RecommendedRunbook)
			}
		})
	}
}

// TestSchemaRejectsExtraFieldsAndBadIdentifiers is the Go port of
// test_schema_rejects_extra_fields_and_bad_ids.
func TestSchemaRejectsExtraFieldsAndBadIdentifiers(t *testing.T) {
	t.Parallel()

	payload := validReport(t)
	surprising := map[string]any{}
	if err := json.Unmarshal([]byte(encode(t, payload)), &surprising); err != nil {
		t.Fatalf("json.Unmarshal() error = %v, want nil", err)
	}
	surprising["surprise"] = true

	bad := payload
	bad.IncidentID = "ticket-2"

	for name, answer := range map[string]string{
		"extra field":        encode(t, surprising),
		"bad incident id":    encode(t, bad),
		"a bare fence":       "```",
		"prose":              "The incident looks bad, please restart the service.",
		"two documents":      encode(t, payload) + encode(t, payload),
		"empty":              "",
		"fence with no body": "```json\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := ParseTriageReport(answer); err == nil {
				t.Error("ParseTriageReport() error = nil, want a validation failure")
			}
		})
	}
}

// TestFirstValidAnswerNeedsNoRetry is the Go port of
// test_first_valid_answer_needs_no_retry.
func TestFirstValidAnswerNeedsNoRetry(t *testing.T) {
	t.Parallel()

	payload := validReport(t)
	var prompts []string
	result, err := RequestTriageReport(t.Context(), ReportRequest{
		Generate: func(_ context.Context, prompt string) (string, error) {
			prompts = append(prompts, prompt)
			return encode(t, payload), nil
		},
	}, payload.IncidentID)
	if err != nil {
		t.Fatalf("RequestTriageReport() error = %v, want nil", err)
	}
	if result.Degraded {
		t.Error("Degraded = true, want false")
	}
	if string(result.Report.IncidentID()) != payload.IncidentID {
		t.Errorf("IncidentID() = %q, want %q", result.Report.IncidentID(), payload.IncidentID)
	}
	if len(prompts) != 1 {
		t.Fatalf("the model was called %d times, want 1", len(prompts))
	}
	for _, want := range []string{"JSON object matching this schema", payload.IncidentID} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("prompt = %q, want it to contain %q", prompts[0], want)
		}
	}
}

// TestViolationRetriesWithTheErrorsFedBack is the Go port of
// test_violation_retries_with_the_errors_fed_back.
//
// The retry is only worth making because it names the offending field: a model
// told "that was invalid" learns nothing, while a model told "incident_id" can
// fix it.
func TestViolationRetriesWithTheErrorsFedBack(t *testing.T) {
	t.Parallel()

	payload := validReport(t)
	bad := payload
	bad.IncidentID = "not-an-id"

	var prompts []string
	result, err := RequestTriageReport(t.Context(), ReportRequest{
		Generate: func(_ context.Context, prompt string) (string, error) {
			prompts = append(prompts, prompt)
			if len(prompts) == 1 {
				return encode(t, bad), nil
			}
			return encode(t, payload), nil
		},
	}, payload.IncidentID)
	if err != nil {
		t.Fatalf("RequestTriageReport() error = %v, want nil", err)
	}
	if result.Degraded {
		t.Error("Degraded = true, want false")
	}
	if len(prompts) != 2 {
		t.Fatalf("the model was called %d times, want 2", len(prompts))
	}
	for _, want := range []string{"failed schema validation", "incident_id"} {
		if !strings.Contains(prompts[1], want) {
			t.Errorf("retry prompt = %q, want it to contain %q", prompts[1], want)
		}
	}
}

// TestDoubleViolationDegradesToProseAndCounts is the Go port of
// test_double_violation_degrades_to_prose_and_counts.
func TestDoubleViolationDegradesToProseAndCounts(t *testing.T) {
	t.Parallel()

	const prose = "The incident looks bad, please restart the affected service."

	counted := 0
	logged := &recordingHandler{}
	calls := 0
	result, err := RequestTriageReport(t.Context(), ReportRequest{
		Generate: func(context.Context, string) (string, error) {
			calls++
			return prose, nil
		},
		Logger:              slog.New(logged),
		RecordSchemaFailure: func(context.Context) { counted++ },
	}, domain.Reference().Incidents.InventoryDown)
	if err != nil {
		t.Fatalf("RequestTriageReport() error = %v, want nil", err)
	}
	if !result.Degraded {
		t.Error("Degraded = false, want true")
	}
	if result.Prose != prose {
		t.Errorf("Prose = %q, want %q", result.Prose, prose)
	}
	if result.Report.IncidentID() != "" || len(result.Report.Evidence()) != 0 {
		t.Error("Report is populated on a degraded result; it must stay zero")
	}
	if calls != 2 {
		t.Errorf("the model was called %d times, want exactly 2: one retry, never more", calls)
	}
	if counted != 1 {
		t.Errorf("schema failures counted = %d, want 1", counted)
	}
	if !strings.Contains(logged.messages(), "degrading to prose") {
		t.Errorf("log = %q, want a degradation warning", logged.messages())
	}
}

// TestDegradationIsSilentWithoutASeam proves the recorder and the logger are
// optional: the policy still degrades, it is simply unpublished.
func TestDegradationIsSilentWithoutASeam(t *testing.T) {
	t.Parallel()

	result, err := RequestTriageReport(t.Context(), ReportRequest{
		Generate: func(context.Context, string) (string, error) { return "not json", nil },
	}, domain.Reference().Incidents.InventoryDown)
	if err != nil {
		t.Fatalf("RequestTriageReport() error = %v, want nil", err)
	}
	if !result.Degraded {
		t.Error("Degraded = false, want true")
	}
}

// TestGeneratorFailuresSurface pins the difference between a model that answers
// badly and a model that does not answer: the first degrades, the second is an
// error the caller has to see.
func TestGeneratorFailuresSurface(t *testing.T) {
	t.Parallel()

	unreachable := errors.New("model unreachable")
	payload := validReport(t)
	bad := payload
	bad.IncidentID = "not-an-id"

	for name, generate := range map[string]Generator{
		"first attempt": func(context.Context, string) (string, error) { return "", unreachable },
		"retry": func() Generator {
			calls := 0
			return func(context.Context, string) (string, error) {
				calls++
				if calls == 1 {
					return encode(t, bad), nil
				}
				return "", unreachable
			}
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := RequestTriageReport(t.Context(), ReportRequest{Generate: generate}, payload.IncidentID)
			if !errors.Is(err, unreachable) {
				t.Errorf("RequestTriageReport() error = %v, want it to wrap the model failure", err)
			}
		})
	}
}

// TestMissingGeneratorIsRefused keeps the seam required rather than optional: a
// nil generator would otherwise panic inside the retry policy.
func TestMissingGeneratorIsRefused(t *testing.T) {
	t.Parallel()

	_, err := RequestTriageReport(t.Context(), ReportRequest{}, domain.Reference().Incidents.InventoryDown)
	if !errors.Is(err, ErrIncompleteConfig) {
		t.Errorf("RequestTriageReport() error = %v, want ErrIncompleteConfig", err)
	}
}

// TestReportAgentEnforcesTheSchema is the Go port of
// test_report_agent_enforces_the_schema.
//
// The tool list order is the read order the instruction prescribes, and the
// output schema coexists with tools: ADK Go hands the schema to the provider
// natively for the OpenAI-compatible path, so the report agent keeps its reads
// during the thought loop exactly as the Python one did.
func TestReportAgentEnforcesTheSchema(t *testing.T) {
	t.Parallel()

	cfg, err := newCompose(t).reportConfig()
	if err != nil {
		t.Fatalf("reportConfig() error = %v, want nil", err)
	}
	if cfg.Name != ReportAgentName {
		t.Errorf("Name = %q, want %q", cfg.Name, ReportAgentName)
	}
	want := []string{tools.GetIncidentToolName, tools.SearchServiceLogsToolName, GetRunbookToolName}
	if got := toolNames(cfg.Tools); !reflect.DeepEqual(got, want) {
		t.Errorf("tools = %v, want %v", got, want)
	}
	if cfg.OutputSchema == nil {
		t.Fatal("OutputSchema = nil; the report's whole point is a validated answer")
	}
	if cfg.OutputSchema.Title != triageReportSchemaTitle {
		t.Errorf("OutputSchema.Title = %q, want %q", cfg.OutputSchema.Title, triageReportSchemaTitle)
	}

	for _, phrase := range []string{
		"Call get_incident first", "exact service and runbook fields", "no query filter", "that order",
		"from tool output only", "never invent ids", "JSON object only", "no Markdown fences",
	} {
		if !strings.Contains(ReportInstruction, phrase) {
			t.Errorf("instruction lost the phrase %q the evalsets depend on", phrase)
		}
	}

	built, err := newCompose(t).TriageReportAgent()
	if err != nil {
		t.Fatalf("TriageReportAgent() error = %v, want nil", err)
	}
	if built.Name() != ReportAgentName || built.Description() != ReportDescription {
		t.Errorf("agent = %q/%q, want %q/%q",
			built.Name(), built.Description(), ReportAgentName, ReportDescription)
	}
}

// TestReportAgentIsIndependentOfTheEntrypoint is the Go port of
// test_structured_report_discovery_exports_a_governed_app.
//
// The Python track exposed the report through its own ADK discovery package so
// an evaluation could drive the schema path without changing AGENT_ENTRYPOINT.
// Here the same property is a method on the composer: it builds on every
// entrypoint, and the policy plugin governs it because the plugin is attached at
// the runner rather than per agent.
func TestReportAgentIsIndependentOfTheEntrypoint(t *testing.T) {
	t.Parallel()

	for _, entrypoint := range configEntrypoints() {
		t.Run(string(entrypoint), func(t *testing.T) {
			t.Parallel()

			composer := newCompose(t, func(cfg *Config) { cfg.Entrypoint = entrypoint })
			report, err := composer.TriageReportAgent()
			if err != nil {
				t.Fatalf("TriageReportAgent() error = %v, want nil", err)
			}
			if report.Name() != ReportAgentName {
				t.Errorf("Name() = %q, want %q", report.Name(), ReportAgentName)
			}
		})
	}
}

// recordingHandler is a slog.Handler that keeps the messages it was given.
type recordingHandler struct{ records []string }

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.records = append(h.records, record.Message)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) messages() string { return strings.Join(h.records, "\n") }
