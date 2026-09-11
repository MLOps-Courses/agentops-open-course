package compose

import (
	"fmt"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
)

// A bounded plan → investigate → evidence review → recommend workflow
// (Chapter 3.5).
//
// Where the conversational agent decides its own steps, a workflow makes the
// operating loop explicit. Each node receives its predecessor's output as
// input, and the fixed chain bounds the topology to four read-only stages. It
// is deliberately not a reflection loop: the graph cannot revisit a stage, so
// the number of model calls one run costs is known before it starts.
//
// ADK Go's workflow runtime is richer than the Python one — conditional
// routing, fan-out and fan-in, parallel workers, per-node retries and timeouts,
// pause and resume — and none of that is used here on purpose. The lesson of
// this chapter is the bound, and every extra edge would weaken it.

// The four stage names, in execution order.
const (
	// PlanStageName turns the request into an investigation contract.
	PlanStageName = "plan"
	// InvestigateStageName collects the evidence the plan named.
	InvestigateStageName = "investigate"
	// EvidenceReviewStageName challenges that evidence before advice exists.
	EvidenceReviewStageName = "evidence_review"
	// RecommendStageName proposes a bounded next step.
	RecommendStageName = "recommend"
)

// WorkflowDescription is what the A2A agent card and the launcher show for the
// graph as a whole.
const WorkflowDescription = "Runs a bounded plan → investigate → evidence review → recommend loop."

// Stage descriptions.
const (
	planDescription = "Defines a concise evidence plan for one incident investigation."

	investigateDescription = "Collects read-only evidence for the planned incident investigation."

	evidenceReviewDescription = "Checks whether incident evidence supports a safe recommendation."

	recommendDescription = "Recommends concrete, runbook-backed remediation."
)

// The four stage instructions are pinned through direct behavioral invariants:
// bounded planning, ordered evidence, explicit uncertainty, and read-only advice.
const (
	planInstruction = "Turn the request into an investigation plan with at most four bullets. Preserve the exact incident " +
		"or service named by the user. If none is named, plan to select the most urgent unresolved incident. " +
		"Use only this evidence frame: incident record, affected service status, service logs, and the exact " +
		"runbook linked by the incident record. Do not invent symptoms, causes, systems, time windows, " +
		"hypotheses, or recovery facts absent from the request. Do not diagnose or recommend. State the " +
		"condition for stopping or escalating."

	investigateInstruction = "The plan controls which evidence to collect; it is not evidence. Execute it for one incident. If it " +
		"has no incident id, call list_incidents, filtering by the exact named service when present, and " +
		"select the most urgent unresolved incident (lowest SEV number). Call get_incident for the exact id. " +
		"Derive the service and runbook slug only from that record, then call get_service_status with the " +
		"exact service, search_service_logs with that service and no query filter, and get_runbook with the " +
		"exact linked slug, in that order. Do not introduce another domain or substitute fuzzy runbook " +
		"search. Return only concise observed evidence with source names; label any inference and stop " +
		"plainly when required evidence is missing. Preserve the exact incident id, service, and runbook slug " +
		"in the handoff, together with observed service status, unfiltered log evidence, and relevant runbook " +
		"guidance."

	evidenceReviewInstruction = "Treat the handed-off investigation as claims, not source truth. When needed to resolve one material " +
		"gap, re-read only its exact incident with get_incident, service with get_service_status, unfiltered " +
		"service logs with search_service_logs, or runbook slug with get_runbook; never discover or invent a " +
		"replacement. Separate observations from inferences and name missing or conflicting evidence. Return " +
		"a compact handoff containing the exact incident id, service, runbook slug, at most four key " +
		"observations, remaining gaps, and a supported, insufficient, or conflicting verdict with one short " +
		"reason. If an exact identifier or required source is absent, return an explicit insufficient verdict " +
		"and name it; never return an opaque error. Do not recommend or take an action."

	recommendInstruction = "Use only the evidence-review handoff and the exact runbook slug it names; never discover an " +
		"alternative. Recommend only what its verdict supports. If the verdict is insufficient or " +
		"conflicting, ask for the missing check instead of proposing a write. Otherwise call get_runbook with " +
		"the handed-off slug, then give at most three runbook-backed next steps, the expected recovery " +
		"evidence, and a rollback or stop condition, preserving the exact incident and service from the " +
		"handoff. Flag restart_service or resolve_incident as requiring human approval; never call either " +
		"action. Cite the handed-off runbook slug."
)

// workflowStageConfigs returns the four stages in execution order.
//
// Each stage receives only the exact reads it needs. Review may verify named
// sources and recommendation may reload the handed-off runbook, but neither can
// reopen discovery and drift to a different target: list_incidents is bound to
// the investigate stage alone, and it is last in that stage's list so the model
// reaches for it only when the plan named no incident.
//
// No stage holds a write. That is the whole claim of a read-only workflow, and
// it is enforced by the absence of the tools rather than by the instruction
// that also asks for it.
func (c *Compose) workflowStageConfigs() []llmagent.Config {
	plan := c.baseConfig(PlanStageName, planDescription, planInstruction)

	investigate := c.baseConfig(InvestigateStageName, investigateDescription, investigateInstruction)
	investigate.Tools = []tool.Tool{
		c.tools.GetIncident,
		c.tools.GetServiceStatus,
		c.tools.SearchServiceLogs,
		c.tools.GetRunbook,
		c.tools.ListIncidents,
	}

	evidenceReview := c.baseConfig(EvidenceReviewStageName, evidenceReviewDescription, evidenceReviewInstruction)
	evidenceReview.Tools = []tool.Tool{
		c.tools.GetIncident,
		c.tools.GetServiceStatus,
		c.tools.SearchServiceLogs,
		c.tools.GetRunbook,
	}

	recommend := c.baseConfig(RecommendStageName, recommendDescription, recommendInstruction)
	recommend.Tools = []tool.Tool{c.tools.GetRunbook}

	return []llmagent.Config{plan, investigate, evidenceReview, recommend}
}

// TriageWorkflow builds the bounded read-only graph.
//
// The graph is a chain: workflow.Chain turns the node list into the pairwise
// edges ADK Go's runtime takes, starting from the package-level Start sentinel,
// and each stage's output becomes the next stage's input. The workflow's name
// is also its persistence namespace inside the session state, which is why it
// is the constant every entrypoint keys on rather than a local string.
// --8<-- [start:triage-workflow]
func (c *Compose) TriageWorkflow() (agent.Agent, error) {
	configs := c.workflowStageConfigs()

	// The Start sentinel plus one node per stage; Chain then produces one edge
	// per adjacent pair.
	nodes := make([]workflow.Node, 0, len(configs)+1)
	nodes = append(nodes, workflow.Start)
	for _, cfg := range configs {
		stage, err := newAgent(cfg)
		if err != nil {
			return nil, err
		}
		// NewAgentNode takes the node's name and description from the agent
		// itself, so the stage names above are the graph's node names too. It
		// also leaves the agent in single-turn mode, which is what makes a
		// stage hand off instead of opening a conversation.
		node, err := workflow.NewAgentNode(stage, workflow.NodeConfig{})
		if err != nil {
			return nil, fmt.Errorf("building the %s workflow node: %w", cfg.Name, err)
		}
		nodes = append(nodes, node)
	}

	built, err := workflowagent.New(workflowagent.Config{
		Name:        WorkflowName,
		Description: WorkflowDescription,
		Edges:       workflow.Chain(nodes...),
	})
	if err != nil {
		return nil, fmt.Errorf("building the %s graph: %w", WorkflowName, err)
	}
	return built, nil
}

// --8<-- [end:triage-workflow]
