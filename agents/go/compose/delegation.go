package compose

import (
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
)

// Multi-agent delegation — a coordinator with least-privilege specialists
// (Chapter 3.7).
//
// Sub-agents are how one agent hands work to another: the coordinator triages,
// then *delegates* by transferring control, and ADK routes to the named
// sub-agent, which shares the session. The security lesson is per-agent least
// privilege, and it is enforced by construction rather than by prompt: the
// diagnosis specialist holds only read tools, so a prompt injection in a log
// line cannot make it mutate state — it has no write tool to call. The
// remediation specialist holds only the guarded writes, so it cannot read raw
// logs, and every action still pauses for human confirmation.
//
// The same wiring has a distributed form this repository does not ship. ADK's
// agent/remoteagent.NewA2A builds an agent.Agent backed by a remote A2A card, and
// a coordinator would take it as a sub-agent exactly as it takes these two — the
// SubAgents assignment below would not change at all. It is left unbuilt on
// purpose: a network hop between two agents this course starts as one process
// buys discovery, authentication, versioning, and a second deployment to operate,
// and 3.6. A2A argues that you should pay for those only when a split has an
// owner. Naming it here so the option is visible; do not read this comment as a
// description of code that exists.

// Descriptions the delegation router reads when it decides where to transfer.
const (
	diagnosisDescription = "Specialist that diagnoses a specific incident using its runbook and service status."

	remediationDescription = "Specialist that executes approved remediation through the guarded actions."

	coordinatorDescription = "On-call coordinator that triages incidents and delegates diagnosis and remediation."
)

// The three delegation instructions are pinned through direct behavioral
// invariants: least privilege, confirmation, attribution, and post-action proof.
const (
	diagnosisInstruction = "You are a diagnosis specialist. Given an incident id, use get_incident for its details and runbook, " +
		"get_runbook for the runbook body, and get_service_status for the service. Explain the likely root " +
		"cause in a few sentences and cite the runbook. When the coordinator asks for post-action " +
		"verification, re-read the incident and service, compare those observations with the expected " +
		"recovery evidence, and label recovery unverified when they do not prove it. You cannot take actions " +
		"— hand your findings back to the coordinator."

	remediationInstruction = "You are a remediation specialist. The coordinator hands you a diagnosed incident and a " +
		"runbook-backed plan. Call the exact guarded action (restart_service or resolve_incident) so ADK " +
		"creates its confirmation request; the coordinator handoff is not approval. After the engineer " +
		"approves with a rationale, return only the attempted action and audit result; never claim service " +
		"recovery from the action response. Never act without a diagnosis; never invent targets."

	coordinatorInstruction = "You are the on-call coordinator. Triage with list_incidents and get_service_status. When a specific " +
		"incident needs a root-cause analysis, delegate to the diagnosis_agent sub-agent. Once a diagnosis is " +
		"confirmed and the engineer wants to act, delegate to the remediation_agent sub-agent with the " +
		"incident id, runbook-backed plan, and expected recovery evidence. Treat its response as action and " +
		"audit evidence only. Then delegate back to diagnosis_agent to re-read the incident and service and " +
		"compare them with the expected recovery evidence. Summarize the audit and verified observations; " +
		"never claim recovery from the action response alone."
)

// diagnosisConfig is the read-only specialist: the four incident reads plus the
// two runbook reads, and deliberately no write.
//
// It binds local tools even when AGENT_MCP_URL is set. The MCP switch is the
// conversational entrypoint's alone, because a specialist's tool set is the
// boundary the least-privilege claim rests on: letting a remote server decide
// which tools this agent holds would hand that boundary to the server.
func (c *Compose) diagnosisConfig() llmagent.Config {
	cfg := c.baseConfig(DiagnosisName, diagnosisDescription, diagnosisInstruction)
	cfg.Tools = concatTools(c.tools.ReadTools(), c.tools.KnowledgeTools())
	return cfg
}

// remediationConfig is the write-only specialist: it executes a diagnosed,
// runbook-backed plan through the guarded actions — each of which still pauses
// for human approval with a rationale — and holds no log or runbook reader.
func (c *Compose) remediationConfig() llmagent.Config {
	cfg := c.baseConfig(RemediationName, remediationDescription, remediationInstruction)
	cfg.Tools = concatTools(c.tools.ActionTools())
	return cfg
}

// coordinatorConfig triages, then routes — diagnosis first, remediation only
// after.
//
// It holds the four incident reads and no knowledge tools, so it can see what
// is broken but must delegate to read a runbook; and it holds no write tool, so
// acting requires delegation as well.
// --8<-- [start:coordinator-wiring]
func (c *Compose) coordinatorConfig(subAgents []agent.Agent) llmagent.Config {
	cfg := c.baseConfig(CoordinatorName, coordinatorDescription, coordinatorInstruction)
	cfg.Tools = concatTools(c.tools.ReadTools())
	cfg.SubAgents = subAgents
	return cfg
}

// CoordinatorAgent builds the coordinator and its two specialists.
//
// Neither transfer direction is disallowed, so ADK's default topology applies:
// the coordinator can transfer down to either specialist, and a specialist can
// hand control back — which is exactly what the coordinator's instruction asks
// for when it delegates to the diagnosis specialist a second time to verify an
// approved action.
func (c *Compose) CoordinatorAgent() (agent.Agent, error) {
	diagnosis, err := newAgent(c.diagnosisConfig())
	if err != nil {
		return nil, err
	}
	remediation, err := newAgent(c.remediationConfig())
	if err != nil {
		return nil, err
	}
	return newAgent(c.coordinatorConfig([]agent.Agent{diagnosis, remediation}))
}

// --8<-- [end:coordinator-wiring]
