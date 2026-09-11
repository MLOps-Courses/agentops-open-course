package compose

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/domain"
)

// Structured outputs — a typed triage report with explicit fallbacks
// (Chapter 4.0).
//
// Free prose is fine for conversation, but downstream automation needs a
// schema. The report agent carries an output schema so its final answer is
// validated JSON, and tools stay available during the thought loop: ADK Go
// hands the schema to the provider natively when the model is not a Gemini API
// model, so the combination costs nothing here.
//
// The programmatic path handles violations explicitly — retry once with the
// validation errors fed back, then degrade to prose with a counted, logged
// event. Never a silent crash, and never a silently-wrong object.

// ReportDescription is what the A2A agent card shows for the report entrypoint.
const ReportDescription = "Produces a schema-validated triage report for a single incident."

// ReportInstruction is the report agent's evidence and output contract.
const ReportInstruction = "You produce a machine-consumable triage report for one incident.\n" +
	"Call get_incident first. Read its exact service and runbook fields, then call\n" +
	"search_service_logs with that service and no query filter, then get_runbook with\n" +
	"that runbook slug, in that order. Fill every field of the TriageReport schema\n" +
	"from tool output only — never invent ids, services, runbooks, or log lines.\n" +
	"Respond with the JSON object only: no prose, no Markdown fences.\n"

// reportConfig is the report agent as a value.
//
// The tool order is the read order the instruction prescribes — the incident
// first, then that incident's logs, then that incident's runbook — so the
// listing a model sees and the sequence it is told to follow agree.
// --8<-- [start:report-config]
func (c *Compose) reportConfig() (llmagent.Config, error) {
	schema, err := TriageReportSchema()
	if err != nil {
		return llmagent.Config{}, err
	}
	cfg := c.baseConfig(ReportAgentName, ReportDescription, ReportInstruction)
	cfg.Tools = []tool.Tool{c.tools.GetIncident, c.tools.SearchServiceLogs, c.tools.GetRunbook}
	cfg.OutputSchema = schema
	return cfg, nil
}

// --8<-- [end:report-config]

// TriageReportAgent builds the structured-report entrypoint.
//
// It is a second, independent composition rather than a mode of the
// conversational agent: the Python track exposed it through its own ADK
// discovery package (structured_report/) so an evaluation could drive the
// schema path without touching AGENT_ENTRYPOINT, and the same separation holds
// here. The application-level policy plugin governs it exactly as it governs
// every other composition, because the plugin is attached at the runner, not
// per agent.
func (c *Compose) TriageReportAgent() (agent.Agent, error) {
	cfg, err := c.reportConfig()
	if err != nil {
		return nil, err
	}
	return newAgent(cfg)
}

// ReportPrompt builds the structured-output request for one incident.
//
// The schema travels in the prompt as well as in the request configuration
// because this function is also the boundary a bare model call uses:
// [RequestTriageReport] is driven through a prompt-in, text-out [Generator]
// that has no ADK request to attach a schema to.
func ReportPrompt(incidentID string) (string, error) {
	schema, err := TriageReportJSONSchema()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		"Produce the triage report for incident %s. "+
			"Respond with a single JSON object matching this schema exactly:\n%s",
		incidentID, schema,
	), nil
}

// ParseTriageReport parses model text into a validated report.
//
// It tolerates the one formatting quirk local models add anyway — a Markdown
// code fence, with or without a language tag — and lets every real violation
// surface as an error. Tolerating more would be indistinguishable from
// accepting a model that is not following the schema.
func ParseTriageReport(text string) (domain.TriageReport, error) {
	cleaned := strings.TrimSpace(text)
	if after, found := strings.CutPrefix(cleaned, "```"); found {
		// Drop the opening fence and its language tag, which run to the end of
		// the line; a fence with no newline at all leaves nothing behind, which
		// then fails validation as it should.
		if _, body, hasNewline := strings.Cut(after, "\n"); hasNewline {
			cleaned = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(body), "```"))
		} else {
			cleaned = ""
		}
	}
	return domain.ParseTriageReport([]byte(cleaned))
}

// Generator is this package's bare-model report boundary: prompt in, text out.
//
// Injecting it is what makes the fallback policy deterministic to test offline
// with a fake, which is the same reason the Python track took a coroutine here
// instead of reaching for a runner.
type Generator func(ctx context.Context, prompt string) (string, error)

// SchemaFailureRecorder publishes one triage report that failed validation
// twice.
//
// It is the seam the telemetry package fills. The Python track incremented an
// OTel counter named agentops.triage_report.schema_failures (unit "1"), which
// is a model-quality signal dashboards watch; that name is the exporter's
// contract, not this rule's. Nil still degrades to prose, just uncounted.
type SchemaFailureRecorder func(ctx context.Context)

// ReportRequest is one structured-report request and its fallback policy.
type ReportRequest struct {
	// Generate is the model boundary. Required.
	Generate Generator
	// Logger receives the degradation warning. Nil uses slog.Default.
	Logger *slog.Logger
	// RecordSchemaFailure counts a double violation. See [SchemaFailureRecorder].
	RecordSchemaFailure SchemaFailureRecorder
}

// ReportResult is what one request produced: either a validated report, or the
// prose the model insisted on.
//
// Both fields are carried rather than returning `any`, so a caller cannot
// mistake a degraded answer for a validated one — reading Report without
// checking Degraded does not compile into a wrong result, it reads a zero
// report. The Python signature was `TriageReport | str`, which needed a runtime
// type check at every call site.
type ReportResult struct {
	// Prose is the model's unvalidated answer. Meaningful only when Degraded.
	Prose string
	// Report is the validated report. Meaningful only when Degraded is false.
	Report domain.TriageReport
	// Degraded reports that both attempts failed validation.
	Degraded bool
}

// RequestTriageReport asks for a validated report, retries once on violation,
// then degrades to prose.
//
// Exactly one retry, never more: a model that cannot produce the schema twice
// in a row will not produce it on the third attempt either, and an unbounded
// loop here would turn a formatting problem into a cost problem.
func RequestTriageReport(
	ctx context.Context, request ReportRequest, incidentID string,
) (ReportResult, error) {
	if request.Generate == nil {
		return ReportResult{}, fmt.Errorf("%w: ReportRequest.Generate is required", ErrIncompleteConfig)
	}
	prompt, err := ReportPrompt(incidentID)
	if err != nil {
		return ReportResult{}, err
	}

	first, err := request.Generate(ctx, prompt)
	if err != nil {
		return ReportResult{}, fmt.Errorf("requesting the triage report for %s: %w", incidentID, err)
	}
	report, parseErr := ParseTriageReport(first)
	if parseErr == nil {
		return ReportResult{Report: report}, nil
	}

	// The retry feeds the validation errors back verbatim. They name the
	// offending field, which is the whole reason a second attempt can succeed
	// where the first did not.
	retryPrompt := fmt.Sprintf(
		"%s\n\nYour previous reply failed schema validation:\n%s\n"+
			"Fix these errors and respond with only the corrected JSON object.",
		prompt, parseErr,
	)
	second, err := request.Generate(ctx, retryPrompt)
	if err != nil {
		return ReportResult{}, fmt.Errorf("retrying the triage report for %s: %w", incidentID, err)
	}
	report, parseErr = ParseTriageReport(second)
	if parseErr == nil {
		return ReportResult{Report: report}, nil
	}

	if request.RecordSchemaFailure != nil {
		request.RecordSchemaFailure(ctx)
	}
	logger := request.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.WarnContext(ctx,
		"triage report failed schema validation twice; degrading to prose",
		slog.String("incident_id", incidentID),
		slog.String("error", parseErr.Error()),
	)
	return ReportResult{Prose: second, Degraded: true}, nil
}
