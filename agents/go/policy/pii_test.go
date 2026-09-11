package policy

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/data"
)

// These tests pin the always-on deterministic floor. Named-entity behavior is
// tested separately at the private agentgateway webhook boundary.

// The addresses the suite redacts. They are invented, not seeded, so they carry
// no domain vocabulary.
const (
	operatorEmail = "jane.doe@acme.com"
	reporterEmail = "alice.smith@example.com"
	failingHost   = "10.0.0.5"
)

// layerOne is the always-on account-free deterministic policy.
func layerOne(t *testing.T) *Policy {
	t.Helper()
	return newPolicy(t, Config{SanitizeToolOutput: true})
}

func TestRedactsEmailAndPhone(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryText(t.Context(),
		"Ping the on-call at "+operatorEmail+" or 555-123-4567.")

	if strings.Contains(redacted, operatorEmail) {
		t.Errorf("RedactBoundaryText() = %q, still contains the address", redacted)
	}
	if strings.Contains(redacted, "555-123-4567") {
		t.Errorf("RedactBoundaryText() = %q, still contains the number", redacted)
	}
	if !strings.Contains(redacted, entityEmail.mask()) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, entityEmail.mask())
	}
}

// TestLayerOneCoversEveryGatewayBuiltin pins the deterministic floor against
// agentgateway 1.4.1's exact builtin inventory. The gateway is optional and is
// introduced later in the course, so none of these classes may depend on it.
func TestLayerOneCoversEveryGatewayBuiltin(t *testing.T) {
	t.Parallel()

	for _, candidate := range []struct {
		name string
		text string
		want entity
	}{
		{name: "US SSN", text: "SSN 123-45-6789", want: entitySSN},
		{name: "US SSN compact", text: "SSN 123456789", want: entitySSN},
		{name: "US SSN five-four", text: "SSN 12345-6789", want: entitySSN},
		{name: "US SSN three-six", text: "SSN 123-456789", want: entitySSN},
		{name: "credit card", text: "card 4111 1111 1111 1111", want: entityCreditCard},
		{name: "phone number", text: "call 555-123-4567", want: entityPhone},
		{name: "email", text: "email jane.doe@example.com", want: entityEmail},
		{name: "Canadian SIN", text: "SIN 046-454-286", want: entityCASIN},
		{name: "Canadian SIN compact", text: "SIN 046454286", want: entityCASIN},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			t.Parallel()

			redacted := layerOne(t).RedactBoundaryText(t.Context(), candidate.text)
			if !strings.Contains(redacted, candidate.want.mask()) {
				t.Errorf("RedactBoundaryText(%q) = %q, want it to contain %s",
					candidate.text, redacted, candidate.want.mask())
			}
		})
	}
}

func TestRedactsIPAddress(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryText(t.Context(),
		"The failing host is "+failingHost+" in the "+checkoutService+" cluster.")

	if strings.Contains(redacted, failingHost) {
		t.Errorf("RedactBoundaryText() = %q, still contains the address", redacted)
	}
	if !strings.Contains(redacted, entityIP.mask()) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, entityIP.mask())
	}
}

// TestBoundaryPolicyRetainsBroadPersonalDataCoverage pins the one class the
// boundary policy carries that the persisted policy does not.
func TestBoundaryPolicyRetainsBroadPersonalDataCoverage(t *testing.T) {
	t.Parallel()

	policy := layerOne(t)
	const mac = "00:11:22:33:44:55"

	redacted := policy.RedactBoundaryText(t.Context(), "The device MAC address is "+mac+".")
	if strings.Contains(redacted, mac) {
		t.Errorf("RedactBoundaryText() = %q, still contains the address", redacted)
	}
	if !strings.Contains(redacted, entityMAC.mask()) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, entityMAC.mask())
	}
	if persisted := policy.RedactPersistedText(t.Context(), mac); persisted != mac {
		t.Errorf("RedactPersistedText(%q) = %q, want it unchanged: the persisted policy omits the class",
			mac, persisted)
	}
}

// TestRedactsPersonalDataWithoutCorruptingDomainIdentifiers is the layer-1 half
// of the Python case; the "Paris" half is TestLayerTwoRedactsLocations.
func TestRedactsPersonalDataWithoutCorruptingDomainIdentifiers(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryText(t.Context(),
		"Email "+operatorEmail+" from Paris about "+authIncident+".")

	if strings.Contains(redacted, operatorEmail) {
		t.Errorf("RedactBoundaryText() = %q, still contains the address", redacted)
	}
	if !strings.Contains(redacted, authIncident) {
		t.Errorf("RedactBoundaryText() = %q, want the incident identifier preserved", redacted)
	}
}

// TestSafeOperationalTokensInsideEmailDomainsStillRedact is the container-first
// phase of detection: a protected token that lives inside an address is part
// of the address, so protecting it would leave the address readable.
func TestSafeOperationalTokensInsideEmailDomainsStillRedact(t *testing.T) {
	t.Parallel()

	for _, address := range []string{
		"jane@" + strings.ToLower(authIncident) + ".com",
		"jane@v2.4.0.com",
		"jane@sev1.com",
		"jane@p95.com",
	} {
		t.Run(address, func(t *testing.T) {
			t.Parallel()

			redacted := layerOne(t).RedactBoundaryText(t.Context(), "Email "+address)
			if strings.Contains(redacted, address) {
				t.Errorf("RedactBoundaryText() = %q, still contains the address", redacted)
			}
			if !strings.Contains(redacted, entityEmail.mask()) {
				t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, entityEmail.mask())
			}
		})
	}
}

// TestURLsSurviveTheBoundaryIntact guards the reason the URL class was dropped
// from both policies: the recognizer matched only the leading part of an
// internal hostname, and a half-rewritten token still looks like a hostname.
func TestURLsSurviveTheBoundaryIntact(t *testing.T) {
	t.Parallel()

	for _, url := range []string{
		"http://grafana.internal/d/abc",
		"http://agentops-mcp.agentops.svc.cluster.local:8000/mcp",
		"https://" + strings.ToLower(authIncident) + ".com/releases/v2.4.0",
	} {
		t.Run(url, func(t *testing.T) {
			t.Parallel()

			policy := layerOne(t)
			text := "Visit " + url + " for the dashboard"
			if redacted := policy.RedactBoundaryText(t.Context(), text); redacted != text {
				t.Errorf("RedactBoundaryText() = %q, want %q", redacted, text)
			}
			// The persisted policy never redacted URLs, so both boundaries agree.
			if redacted := policy.RedactPersistedText(t.Context(), text); redacted != text {
				t.Errorf("RedactPersistedText() = %q, want %q", redacted, text)
			}
		})
	}
}

// TestCredentialsInsideAURLAreStillRemoved proves dropping the URL class did
// not weaken the tripwires, which run before any entity analysis.
func TestCredentialsInsideAURLAreStillRemoved(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryText(t.Context(),
		"curl https://api.example.com/v1?OPENAI_API_KEY=sk-secret-value")

	if strings.Contains(redacted, "sk-secret-value") {
		t.Errorf("RedactBoundaryText() = %q, still contains the secret", redacted)
	}
	if !strings.Contains(redacted, SecretMask) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, SecretMask)
	}
}

// TestEveryConfiguredEntityHasAnOwner asserts that every Layer 1 class has a
// deterministic recognizer. A class without one would quietly promise nothing.
func TestEveryConfiguredEntityHasAnOwner(t *testing.T) {
	t.Parallel()

	deterministic := map[entity]bool{}
	for _, detector := range recognizers() {
		deterministic[detector.entity] = true
	}
	for _, policy := range []struct {
		name  string
		value redactionPolicy
	}{
		{"boundary", boundaryPolicy()},
		{"persisted", persistedPolicy()},
	} {
		t.Run(policy.name, func(t *testing.T) {
			t.Parallel()

			for _, name := range policy.value.entities {
				if !deterministic[name] {
					t.Errorf("entity %s has no deterministic recognizer", name)
				}
			}
		})
	}
}

// TestLayerOneDoesNotDetectNames states the boundary out loud.
//
// Person, location and nationality detection is named-entity recognition, which
// needs a model. Layer 1 does not have one, and approximating it with a name
// list would look like coverage while missing every name the list does not
// contain. The test asserts the honest behavior so the gap is visible in the
// suite rather than only in the prose.
func TestLayerOneDoesNotDetectNames(t *testing.T) {
	t.Parallel()

	policy := layerOne(t)
	text := "John " + authIncident
	if redacted := policy.RedactBoundaryText(t.Context(), text); redacted != text {
		t.Errorf("RedactBoundaryText() = %q, want %q unchanged; layer 1 has no NER", redacted, text)
	}
}

func TestKeepsServiceAndRunbookIdentifiers(t *testing.T) {
	t.Parallel()

	text := "Search the " + inventoryService + " logs and open " + serviceDownBook + " for " + checkoutIncident + "."
	if redacted := layerOne(t).RedactBoundaryText(t.Context(), text); redacted != text {
		t.Errorf("RedactBoundaryText() = %q, want %q", redacted, text)
	}
}

func TestKeepsVersionsWhenTheEntityModelGroupsThePrefix(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"release", "version"} {
		t.Run(prefix, func(t *testing.T) {
			t.Parallel()

			text := prefix + " v2026.07.07-2"
			if redacted := layerOne(t).RedactBoundaryText(t.Context(), text); redacted != text {
				t.Errorf("RedactBoundaryText() = %q, want %q", redacted, text)
			}
		})
	}
}

func TestKeepsExactIncidentAndSeverityIdentifiers(t *testing.T) {
	t.Parallel()

	for _, identifier := range []string{checkoutIncident, authIncident, "SEV1", "SEV2", "SEV3"} {
		t.Run(identifier, func(t *testing.T) {
			t.Parallel()

			policy := layerOne(t)
			if redacted := policy.RedactBoundaryText(t.Context(), identifier); redacted != identifier {
				t.Errorf("RedactBoundaryText() = %q, want %q", redacted, identifier)
			}
			if redacted := policy.RedactPersistedText(t.Context(), identifier); redacted != identifier {
				t.Errorf("RedactPersistedText() = %q, want %q", redacted, identifier)
			}
		})
	}
}

// TestRealToolPayloadKeepsOperationalEvidence runs the redactor over the
// committed dataset rather than over a hand-written sample, because the
// evidence tokens that must survive — percentiles, release tags, timestamps —
// are the ones the seed actually contains.
func TestRealToolPayloadKeepsOperationalEvidence(t *testing.T) {
	t.Parallel()

	store := data.New(data.Config{DataDir: repositoryDataset, StateDir: t.TempDir()})
	incidents, err := store.ListIncidents(t.Context(), data.IncidentFilter{})
	if err != nil {
		t.Fatalf("ListIncidents() error = %v, want nil", err)
	}
	if len(incidents) == 0 {
		t.Fatal("the committed dataset holds no incidents")
	}

	rows := make([]any, 0, len(incidents))
	for _, incident := range incidents {
		row := incident.Row()
		rows = append(rows, map[string]any{
			"id":        row.ID,
			"severity":  row.Severity,
			"summary":   row.Summary,
			"opened_at": row.OpenedAt,
		})
	}
	payload := map[string]any{"incidents": rows, "operator_email": operatorEmail}

	rendered := renderValue(layerOne(t).RedactBoundaryValue(t.Context(), payload))
	for _, evidence := range []string{
		cascadeIncident,
		"SEV2",
		"p95",
		"p99",
		"v2026.07.07-2",
		"2026-07-08T06:30:00Z",
	} {
		if !strings.Contains(rendered, evidence) {
			t.Errorf("redacted payload lost the evidence %q:\n%s", evidence, rendered)
		}
	}
	if strings.Contains(rendered, operatorEmail) {
		t.Errorf("redacted payload still contains the address:\n%s", rendered)
	}
	if !strings.Contains(rendered, entityEmail.mask()) {
		t.Errorf("redacted payload does not contain %s:\n%s", entityEmail.mask(), rendered)
	}
}

func TestLeavesPIIFreeTextUnchanged(t *testing.T) {
	t.Parallel()

	clean := "List the open incidents for the " + checkoutService + " service."
	if redacted := layerOne(t).RedactBoundaryText(t.Context(), clean); redacted != clean {
		t.Errorf("RedactBoundaryText() = %q, want %q", redacted, clean)
	}
}

func TestPersistedTextRedactsPIIAndCredentialsButKeepsDomainIDs(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactPersistedText(t.Context(),
		authIncident+" approved by "+operatorEmail+" with Bearer abcdefghijklmnop and sk-abcdefghijklmnop")

	if !strings.Contains(redacted, authIncident) {
		t.Errorf("RedactPersistedText() = %q, want the incident identifier preserved", redacted)
	}
	if strings.Contains(redacted, operatorEmail) {
		t.Errorf("RedactPersistedText() = %q, still contains the address", redacted)
	}
	if strings.Contains(redacted, "abcdefghijklmnop") {
		t.Errorf("RedactPersistedText() = %q, still contains a credential", redacted)
	}
	if !strings.Contains(redacted, entityEmail.mask()) {
		t.Errorf("RedactPersistedText() = %q, want it to contain %s", redacted, entityEmail.mask())
	}
	if got := strings.Count(redacted, SecretMask); got != 2 {
		t.Errorf("RedactPersistedText() = %q, %s appears %d times, want 2", redacted, SecretMask, got)
	}
}

func TestBoundaryPolicyRedactsCredentials(t *testing.T) {
	t.Parallel()

	for _, credential := range []struct {
		name    string
		secret  string
		visible string
	}{
		{"bearer", "Bearer abcdefghijklmnop", "abcdefghijklmnop"},
		{"provider-token", "sk-abcdefghijklmnop", "abcdefghijklmnop"},
		{"quoted-password", `password="correct horse battery staple"`, "correct horse"},
		{"quoted-access-token", "access_token='long lived credential'", "long lived"},
		{"prefixed-api-key", "OPENAI_API_KEY=plain-secret-value", "plain-secret"},
		{"other-prefixed-api-key", "GOOGLE_API_KEY=another-secret-value", "another-secret"},
		{"prefixed-password", `MY_PASSWORD="correct horse battery staple"`, "correct horse"},
	} {
		t.Run(credential.name, func(t *testing.T) {
			t.Parallel()

			redacted := layerOne(t).RedactBoundaryText(t.Context(),
				"Do not expose "+credential.secret+" to the model.")
			if strings.Contains(redacted, credential.visible) {
				t.Errorf("RedactBoundaryText() = %q, still contains %q", redacted, credential.visible)
			}
			if !strings.Contains(redacted, SecretMask) {
				t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, SecretMask)
			}
		})
	}
}

// TestLabeledSecretsKeepTheirLabel pins the diagnostic half of the mask: an
// engineer reading a transcript must be able to tell which credential leaked.
func TestLabeledSecretsKeepTheirLabel(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryText(t.Context(), "token: abc-123-def")
	if want := "token=" + SecretMask; !strings.Contains(redacted, want) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %q", redacted, want)
	}
}

func TestBlankTextIsUntouchedAtTheBoundary(t *testing.T) {
	t.Parallel()

	const blank = "   "
	if redacted := layerOne(t).RedactBoundaryText(t.Context(), blank); redacted != blank {
		t.Errorf("RedactBoundaryText(%q) = %q, want it unchanged", blank, redacted)
	}
}

func TestRequestCallbackRedactsPartsInPlace(t *testing.T) {
	t.Parallel()

	request := &model.LLMRequest{Contents: []*genai.Content{
		textPart("user", "Email "+operatorEmail+" about "+authIncident+"."),
	}}

	response, err := layerOne(t).RedactRequest(newContext(), request)
	if err != nil {
		t.Fatalf("RedactRequest() error = %v, want nil", err)
	}
	// Returning nil is what lets the now-redacted request proceed to the model.
	if response != nil {
		t.Errorf("RedactRequest() = %v, want nil", response)
	}

	redacted := request.Contents[0].Parts[0].Text
	if strings.Contains(redacted, operatorEmail) {
		t.Errorf("part text = %q, still contains the address", redacted)
	}
	if !strings.Contains(redacted, entityEmail.mask()) {
		t.Errorf("part text = %q, want it to contain %s", redacted, entityEmail.mask())
	}
	if !strings.Contains(redacted, authIncident) {
		t.Errorf("part text = %q, want the incident identifier preserved", redacted)
	}
}

func TestRequestCallbackToleratesContentWithNoParts(t *testing.T) {
	t.Parallel()

	request := &model.LLMRequest{Contents: []*genai.Content{{Role: "user"}, nil}}
	response, err := layerOne(t).RedactRequest(newContext(), request)
	if err != nil || response != nil {
		t.Errorf("RedactRequest() = (%v, %v), want (nil, nil)", response, err)
	}
}

func TestRequestCallbackRedactsStructuredToolResults(t *testing.T) {
	t.Parallel()

	request := &model.LLMRequest{Contents: []*genai.Content{
		resultPart("lookup", map[string]any{
			"owner":  operatorEmail,
			"nested": []any{failingHost},
		}),
	}}

	if _, err := layerOne(t).RedactRequest(newContext(), request); err != nil {
		t.Fatalf("RedactRequest() error = %v, want nil", err)
	}

	rendered := renderValue(request.Contents[0].Parts[0].FunctionResponse.Response)
	for _, leaked := range []string{operatorEmail, failingHost} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("tool result = %s, still contains %q", rendered, leaked)
		}
	}
}

func TestRequestCallbackRedactsFunctionCallArguments(t *testing.T) {
	t.Parallel()

	request := &model.LLMRequest{Contents: []*genai.Content{
		callPart("lookup", map[string]any{"incident_id": authIncident, "owner": operatorEmail}),
	}}

	if _, err := layerOne(t).RedactRequest(newContext(), request); err != nil {
		t.Fatalf("RedactRequest() error = %v, want nil", err)
	}

	args := request.Contents[0].Parts[0].FunctionCall.Args
	if rendered := renderValue(args); strings.Contains(rendered, operatorEmail) {
		t.Errorf("arguments = %s, still contain the address", rendered)
	}
	if args["incident_id"] != authIncident {
		t.Errorf("arguments[incident_id] = %v, want %q", args["incident_id"], authIncident)
	}
}

func TestRequestCallbackRedactsCredentialsInTextAndStructuredValues(t *testing.T) {
	t.Parallel()

	request := &model.LLMRequest{Contents: []*genai.Content{{
		Role: "user",
		Parts: []*genai.Part{
			{Text: `password="correct horse battery staple"`},
			{FunctionResponse: &genai.FunctionResponse{
				Name:     "lookup",
				Response: map[string]any{"authorization": "Bearer abcdefghijklmnop"},
			}},
		},
	}}}

	if _, err := layerOne(t).RedactRequest(newContext(), request); err != nil {
		t.Fatalf("RedactRequest() error = %v, want nil", err)
	}

	rendered := renderContents(request.Contents)
	for _, leaked := range []string{"correct horse", "abcdefghijklmnop"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("request = %s, still contains %q", rendered, leaked)
		}
	}
	if got := strings.Count(rendered, SecretMask); got != 2 {
		t.Errorf("request = %s, %s appears %d times, want 2", rendered, SecretMask, got)
	}
}

// TestRecursiveRedactionPreservesTypedSlicesAndNonStrings is the Go reading of
// the Python tuple case: Python rebuilt a tuple as a tuple and left its
// non-string members alone, so the Go recursion must rebuild a typed slice as a
// typed slice and leave a number alone.
func TestRecursiveRedactionPreservesTypedSlicesAndNonStrings(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactBoundaryValue(t.Context(), map[string]any{
		"lines": []string{operatorEmail, "ERROR pool exhausted"},
		"count": 7,
	})

	value, ok := redacted.(map[string]any)
	if !ok {
		t.Fatalf("RedactBoundaryValue() = %T, want map[string]any", redacted)
	}
	lines, ok := value["lines"].([]string)
	if !ok {
		t.Fatalf("lines = %T, want []string", value["lines"])
	}
	if strings.Contains(lines[0], operatorEmail) {
		t.Errorf("lines[0] = %q, still contains the address", lines[0])
	}
	if lines[1] != "ERROR pool exhausted" {
		t.Errorf("lines[1] = %q, want it unchanged", lines[1])
	}
	if value["count"] != 7 {
		t.Errorf("count = %v, want 7 unchanged", value["count"])
	}
}

func TestPersistedRedactionRecursesThroughSessionAndToolShapes(t *testing.T) {
	t.Parallel()

	redacted := layerOne(t).RedactPersistedValue(t.Context(), map[string]any{
		"session": map[string]any{
			"messages": []any{
				map[string]any{"role": "user", "parts": []string{"Email " + operatorEmail, "seven"}},
				map[string]any{"role": "tool", "response": map[string]string{
					"authorization": "Bearer abcdefghijklmnop",
					"incident_id":   inventoryIncident,
				}},
			},
		},
	})

	rendered := renderValue(redacted)
	for _, leaked := range []string{operatorEmail, "abcdefghijklmnop"} {
		if strings.Contains(rendered, leaked) {
			t.Errorf("redacted = %s, still contains %q", rendered, leaked)
		}
	}
	for _, want := range []string{entityEmail.mask(), SecretMask, inventoryIncident} {
		if !strings.Contains(rendered, want) {
			t.Errorf("redacted = %s, want it to contain %q", rendered, want)
		}
	}
}

// TestPersistedRedactorSatisfiesTheToolsSeam proves the adapter the tools
// package's Redactor seam consumes actually redacts, so an audit rationale
// cannot reach the append-only trail unredacted.
func TestPersistedRedactorSatisfiesTheToolsSeam(t *testing.T) {
	t.Parallel()

	redact := layerOne(t).PersistedRedactor(t.Context())
	redacted := redact(authIncident + " approved by " + operatorEmail + " with token=super-secret-token-123456")

	if !strings.Contains(redacted, authIncident) {
		t.Errorf("redactor output = %q, want the incident identifier preserved", redacted)
	}
	if strings.Contains(redacted, operatorEmail) || strings.Contains(redacted, "super-secret-token") {
		t.Errorf("redactor output = %q, still contains sensitive text", redacted)
	}
	if want := "token=" + SecretMask; !strings.Contains(redacted, want) {
		t.Errorf("redactor output = %q, want it to contain %q", redacted, want)
	}
}

func TestAfterModelCallbackRedactsFinalOutput(t *testing.T) {
	t.Parallel()

	response := &model.LLMResponse{Content: textPart("model", "Email "+operatorEmail)}
	redacted, err := layerOne(t).RedactResponse(newContext(), response, nil)
	if err != nil {
		t.Fatalf("RedactResponse() error = %v, want nil", err)
	}
	// The same object comes back: ADK replaces the response with what it gets.
	if redacted != response {
		t.Errorf("RedactResponse() = %p, want the same response %p", redacted, response)
	}
	if text := response.Content.Parts[0].Text; strings.Contains(text, operatorEmail) {
		t.Errorf("response text = %q, still contains the address", text)
	}
}

func TestAfterModelCallbackRedactsFunctionCallAndSkipsCleanOutput(t *testing.T) {
	t.Parallel()

	policy := layerOne(t)

	dirty := &model.LLMResponse{Content: callPart("lookup", map[string]any{"owner": operatorEmail})}
	redacted, err := policy.RedactResponse(newContext(), dirty, nil)
	if err != nil {
		t.Fatalf("RedactResponse() error = %v, want nil", err)
	}
	if redacted != dirty {
		t.Errorf("RedactResponse() = %p, want the same response %p", redacted, dirty)
	}
	if rendered := renderValue(dirty.Content.Parts[0].FunctionCall.Args); strings.Contains(rendered, operatorEmail) {
		t.Errorf("arguments = %s, still contain the address", rendered)
	}

	for _, clean := range []struct {
		value *model.LLMResponse
		name  string
	}{
		{name: "empty text part", value: &model.LLMResponse{Content: textPart("model", "")}},
		{name: "no content", value: &model.LLMResponse{}},
	} {
		t.Run(clean.name, func(t *testing.T) {
			t.Parallel()

			got, err := policy.RedactResponse(newContext(), clean.value, nil)
			if err != nil || got != nil {
				t.Errorf("RedactResponse() = (%v, %v), want (nil, nil)", got, err)
			}
		})
	}
}

// TestStreamedPartialChunksAreRedactedIndividually proves streaming does not
// bypass redaction: every partial chunk passes the same callback.
func TestStreamedPartialChunksAreRedactedIndividually(t *testing.T) {
	t.Parallel()

	chunk := &model.LLMResponse{
		Partial: true,
		Content: textPart("model", "escalate to "+operatorEmail+" now"),
	}
	redacted, err := layerOne(t).RedactResponse(newContext(), chunk, nil)
	if err != nil {
		t.Fatalf("RedactResponse() error = %v, want nil", err)
	}
	if redacted == nil {
		t.Fatal("RedactResponse() = nil, want the redacted chunk")
	}
	if text := redacted.Content.Parts[0].Text; strings.Contains(text, operatorEmail) {
		t.Errorf("chunk text = %q, still contains the address", text)
	}
}

// TestChunkBoundaryEntitiesAreBestEffortButTheAggregateIsCaught documents the
// limit honestly: an entity split across two chunks may not be detected in
// either fragment, while the aggregate always is. A fragment already streamed
// to a client cannot be retracted, which is why streaming defaults to off.
func TestChunkBoundaryEntitiesAreBestEffortButTheAggregateIsCaught(t *testing.T) {
	t.Parallel()

	first, second := "contact jane.doe@", "acme.com for access"
	aggregate := layerOne(t).RedactBoundaryText(t.Context(), first+second)

	if strings.Contains(aggregate, operatorEmail) {
		t.Errorf("aggregate = %q, still contains the address", aggregate)
	}
	if !strings.Contains(aggregate, entityEmail.mask()) {
		t.Errorf("aggregate = %q, want it to contain %s", aggregate, entityEmail.mask())
	}
}

// TestChecksumsGateTheNumericRecognizers proves the recognizers are more than
// shapes: a run of digits is not a card number and a country-prefixed string is
// not an account number until the checksum agrees.
func TestChecksumsGateTheNumericRecognizers(t *testing.T) {
	t.Parallel()

	policy := layerOne(t)
	for _, candidate := range []struct {
		name     string
		text     string
		expected entity
		redacted bool
	}{
		{name: "valid card", text: "card 4111 1111 1111 1111 on file", expected: entityCreditCard, redacted: true},
		{name: "card failing luhn", text: "card 4111 1111 1111 1112 on file", redacted: false},
		{name: "valid iban", text: "account GB82WEST12345698765432 settled", expected: entityIBAN, redacted: true},
		{name: "iban failing mod 97", text: "account GB82WEST12345698765433 settled", redacted: false},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			t.Parallel()

			redacted := policy.RedactBoundaryText(t.Context(), candidate.text)
			if candidate.redacted {
				if !strings.Contains(redacted, candidate.expected.mask()) {
					t.Errorf("RedactBoundaryText(%q) = %q, want it to contain %s",
						candidate.text, redacted, candidate.expected.mask())
				}
				return
			}
			if redacted != candidate.text {
				t.Errorf("RedactBoundaryText(%q) = %q, want it unchanged: the checksum fails",
					candidate.text, redacted)
			}
		})
	}
}

func TestDocumentValidatorsRejectInvalidChecksumsAndReservedGroups(t *testing.T) {
	t.Parallel()

	for candidate, want := range map[string]bool{
		"046-454-286": true,
		"046-454-287": false,
	} {
		if got := passesCASINChecksum(candidate); got != want {
			t.Errorf("passesCASINChecksum(%q) = %t, want %t", candidate, got, want)
		}
	}
	for candidate, want := range map[string]bool{
		"123-45-6789": true,
		"000-45-6789": false,
		"666-45-6789": false,
		"900-45-6789": false,
		"123-00-6789": false,
		"123-45-0000": false,
	} {
		if got := isValidSSN(candidate); got != want {
			t.Errorf("isValidSSN(%q) = %t, want %t", candidate, got, want)
		}
	}
}

// TestColonSeparatedRunsAreNotAddresses is the regression the netip gate buys:
// a MAC address and a clock time both look like IPv6 to a pattern alone.
func TestColonSeparatedRunsAreNotAddresses(t *testing.T) {
	t.Parallel()

	policy := layerOne(t)
	for _, candidate := range []struct {
		name string
		text string
		want string
	}{
		{"ipv6", "peer fe80::1 is unreachable", entityIP.mask()},
		{"clock time", "restarted at 06:30:00 UTC", ""},
	} {
		t.Run(candidate.name, func(t *testing.T) {
			t.Parallel()

			redacted := policy.RedactBoundaryText(t.Context(), candidate.text)
			if candidate.want == "" {
				if redacted != candidate.text {
					t.Errorf("RedactBoundaryText(%q) = %q, want it unchanged", candidate.text, redacted)
				}
				return
			}
			if !strings.Contains(redacted, candidate.want) {
				t.Errorf("RedactBoundaryText(%q) = %q, want it to contain %s",
					candidate.text, redacted, candidate.want)
			}
		})
	}
}

// contextKeyProbe makes sure the redactor never needs anything from the context
// beyond cancellation, which is what lets the persisted redactor bind one.
func TestRedactionNeedsNothingFromTheContext(t *testing.T) {
	t.Parallel()

	//nolint:usetesting // the point is that a bare context is enough here.
	if redacted := layerOne(t).RedactBoundaryText(context.Background(), operatorEmail); //
	!strings.Contains(redacted, entityEmail.mask()) {
		t.Errorf("RedactBoundaryText() = %q, want it to contain %s", redacted, entityEmail.mask())
	}
}
