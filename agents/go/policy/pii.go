package policy

import (
	"context"
	"net/netip"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
)

// This file is layer 1 of the two-layer PII defense (Chapter 4.5): in process,
// always on, no network, no model. It covers every agentgateway builtin plus
// checksum-backed card, Canadian SIN, and IBAN recognition the gateway lacks.
//
// Layer 2 is agentgateway's central promptGuard: its builtins mask every model
// client, while a private Go webhook asks local Ollama for PERSON, LOCATION,
// and ORGANIZATION entities. Layer 1 alone does not detect names. That is a
// stated boundary, not an omission.

// entity is a deterministic class of personal data. The values remain stable
// because they form the visible mask contract.
type entity string

const (
	entityCreditCard entity = "CREDIT_CARD"
	entityEmail      entity = "EMAIL_ADDRESS"
	entityIBAN       entity = "IBAN_CODE"
	entityIP         entity = "IP_ADDRESS"
	entityMAC        entity = "MAC_ADDRESS"
	entityPhone      entity = "PHONE_NUMBER"
	entitySSN        entity = "US_SSN"
	entityCASIN      entity = "CA_SIN"
)

// mask is the placeholder that replaces a detected span.
func (e entity) mask() string { return "<" + string(e) + ">" }

// SecretMask replaces a credential the tripwires below recognize. Unlike an
// entity mask it names no class, because a labeled secret keeps its label:
// an API key assignment becomes NAME=<SECRET>, which tells an engineer which
// credential leaked without leaking it.
const SecretMask = "<SECRET>"

// redactionPolicy is one resolved answer to "which classes do we remove from
// this kind of text, and does blank text pass through untouched?".
type redactionPolicy struct {
	entities []entity
	// blankPassesThrough short-circuits whitespace-only text. The boundary
	// policy does; the persisted policy deliberately does not, because it
	// writes evidence that can never be edited again.
	blankPassesThrough bool
}

// boundaryPolicy governs text crossing the model and tool boundaries.
//
// Two tempting broad classes remain deliberately absent:
//
//   - ORGANIZATION misclassified identifiers such as an incident id and would
//     have corrupted the arguments the agent needs.
//   - URL matched only the leading part of an internal hostname, so
//     http://grafana.internal/d/abc came back as <URL>ernal/d/abc. A partially
//     rewritten token still looks like a hostname, which is worse than either a
//     miss or an over-redaction, and an on-call agent's evidence is full of
//     .internal, .local and .svc.cluster.local names. Credentials embedded in a
//     URL are still caught by the tripwires, which run first.
func boundaryPolicy() redactionPolicy {
	return redactionPolicy{
		entities:           append(persistedPolicy().entities, entityMAC),
		blankPassesThrough: true,
	}
}

// persistedPolicy governs free text that reaches durable local state — an audit
// rationale, a saved note. Audit evidence benefits from keeping generic
// identifiers readable, so the persisted policy stays tighter than the boundary
// one while still removing concrete personal data.
func persistedPolicy() redactionPolicy {
	return redactionPolicy{
		entities: []entity{
			entityCASIN,
			entityCreditCard,
			entityEmail,
			entityIBAN,
			entityIP,
			entityPhone,
			entitySSN,
		},
	}
}

// wants reports whether this policy removes the given class.
func (r redactionPolicy) wants(name entity) bool { return slices.Contains(r.entities, name) }

// credentialPatterns are the pre-analysis tripwires, applied in order. They run
// before any entity analysis so that a credential is masked even when it sits
// inside a construct the entity policy deliberately ignores, such as a URL
// query string.
var credentialPatterns = []struct {
	pattern *regexp.Regexp
	// replacement is expanded by regexp.ReplaceAllString, so ${label} below
	// refers to the captured group. No other replacement contains a dollar sign.
	replacement string
}{
	{
		// A labeled secret. The (?:[A-Za-z][A-Za-z0-9]*_)* prefix is what lets
		// a prefixed environment-variable name match, and keeping the label
		// while normalizing the separator to "=" is what makes the mask
		// diagnostic instead of merely safe.
		pattern: regexp.MustCompile(
			`(?i)\b(?P<label>(?:[A-Za-z][A-Za-z0-9]*_)*` +
				`(?:api[_-]?key|access[_-]?token|auth[_-]?token|password|secret|token))` +
				`\s*[:=]\s*(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s,;]+)`,
		),
		replacement: `${label}=` + SecretMask,
	},
	{
		// An Authorization header value, in any spelling. The replacement
		// re-canonicalizes the scheme so the shape stays recognizable.
		pattern:     regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
		replacement: "Bearer " + SecretMask,
	},
	{
		// Provider-issued token shapes, matched case-sensitively because their
		// prefixes are case-significant.
		pattern: regexp.MustCompile(
			`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|sk-[A-Za-z0-9_-]{16,}|AIza[A-Za-z0-9_-]{20,})\b`,
		),
		replacement: SecretMask,
	},
}

// domainSafeToken matches the operational tokens that must survive analysis
// intact: incident ids, severities, latency percentiles, semver-ish release
// tags and ISO-8601 timestamps.
//
// RE2 has no lookaround, so the surrounding-character rule these tokens need is
// applied by boundedMatches instead of being spelled in the pattern. The
// incident-id shape is duplicated here — domain.ParseIncidentID owns the same
// shape, and a pivot that changes the prefix has to change both.
var domainSafeToken = regexp.MustCompile(
	`(?i)INC-[0-9]+` +
		`|SEV[0-9]+` +
		`|p[0-9]{2,3}` +
		`|v[0-9]+(?:\.[0-9]+)+(?:-[0-9]+)?` +
		`|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:Z|[+-][0-9]{2}:[0-9]{2})`,
)

// recognizer is one deterministic detector: a shape, an optional checksum, and
// the class it reports. The checksum is what separates a recognizer from a
// regex — a sixteen-digit run is not a card number until Luhn says so.
type recognizer struct {
	pattern  *regexp.Regexp
	validate func(match string) bool
	entity   entity
}

// recognizers returns every detector layer 1 can run, in a fixed order so
// overlapping candidates resolve the same way on every call.
//
// The set covers agentgateway 1.4.1's five builtin classes, then adds the
// always-on agent-only classes required at persistence and tool boundaries.
func recognizers() []recognizer {
	return []recognizer{
		{entity: entityEmail, pattern: emailPattern},
		{entity: entityMAC, pattern: macPattern},
		{entity: entityIP, pattern: ipv4Pattern, validate: isIPAddress},
		{entity: entityIP, pattern: ipv6Pattern, validate: isIPAddress},
		{entity: entityIBAN, pattern: ibanPattern, validate: passesIBANChecksum},
		{entity: entityCreditCard, pattern: creditCardPattern, validate: passesLuhn},
		{entity: entityPhone, pattern: phonePattern},
		{entity: entitySSN, pattern: ssnPattern, validate: isValidSSN},
		{entity: entityCASIN, pattern: caSINPattern, validate: passesCASINChecksum},
	}
}

var (
	// emailPattern requires the trailing label to look like a TLD. That is a
	// shape rule, not a registry lookup.
	emailPattern = regexp.MustCompile(
		`(?i)\b[A-Za-z0-9._%+\-]+@` +
			`[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?` +
			`(?:\.[A-Za-z0-9](?:[A-Za-z0-9\-]*[A-Za-z0-9])?)*` +
			`\.[A-Za-z]{2,}\b`,
	)

	// macPattern covers the colon/hyphen form and Cisco's dotted-quad form.
	macPattern = regexp.MustCompile(
		`\b(?:[0-9A-Fa-f]{2}[:-]){5}[0-9A-Fa-f]{2}\b` +
			`|\b(?:[0-9A-Fa-f]{4}\.){2}[0-9A-Fa-f]{4}\b`,
	)

	// ipv4Pattern is octet-accurate, so an out-of-range quad never even reaches
	// the validator.
	ipv4Pattern = regexp.MustCompile(
		`\b(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}` +
			`(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\b`,
	)

	// ipv6Pattern is deliberately loose; netip.ParseAddr is the real gate. That
	// is what keeps a MAC address and a clock time — both colon-separated
	// hex-ish runs — from being reported as addresses.
	ipv6Pattern = regexp.MustCompile(`[0-9A-Fa-f]*(?::[0-9A-Fa-f]*)+(?:%[0-9A-Za-z]+)?`)

	// ibanPattern is the generic IBAN shape; the mod-97 checksum decides.
	ibanPattern = regexp.MustCompile(`\b[A-Z]{2}[0-9]{2}[A-Z0-9]{11,30}\b`)

	// creditCardPattern is a 13-to-19 digit run with optional single spaces or
	// hyphens between digits; Luhn decides.
	creditCardPattern = regexp.MustCompile(`\b(?:[0-9][ \-]?){12,18}[0-9]\b`)

	// phonePattern covers the separated national form and E.164.
	//
	// The separators exclude ":" so a clock time is never a phone number, and
	// the groups are fixed-width so a calendar date is not either.
	phonePattern = regexp.MustCompile(
		`(?:\+[0-9]{1,3}[ .\-]?)?(?:\([0-9]{3}\)[ .\-]?|\b[0-9]{3}[ .\-])[0-9]{3}[ .\-][0-9]{4}\b` +
			`|\+[0-9]{8,15}\b`,
	)

	// ssnPattern covers the canonical U.S. Social Security number form. The
	// structural validator rejects the reserved and all-zero groups that a
	// shape-only gateway builtin accepts.
	ssnPattern = regexp.MustCompile(
		`\b(?:[0-9]{5}-[0-9]{4}|[0-9]{3}-[0-9]{6}|[0-9]{3}[- .][0-9]{2}[- .][0-9]{4}|[0-9]{9})\b`,
	)

	// caSINPattern covers the readable Canadian Social Insurance Number form;
	// the Luhn checksum keeps arbitrary nine-digit operational identifiers out
	// of the always-on redaction path.
	caSINPattern = regexp.MustCompile(`\b(?:[0-9]{3}[- ][0-9]{3}[- ][0-9]{3}|[0-9]{9})\b`)
)

// span is a detected range of text, in byte offsets, and what it holds.
type span struct {
	entity entity
	start  int
	end    int
}

// overlaps reports whether two spans intersect.
func (s span) overlaps(other span) bool { return s.start < other.end && other.start < s.end }

// overlapsAny reports whether s intersects any span in the list.
func (s span) overlapsAny(others []span) bool {
	return slices.ContainsFunc(others, s.overlaps)
}

// redactCredentials masks common credential forms before any text crosses a
// trust boundary.
func redactCredentials(text string) string {
	for _, credential := range credentialPatterns {
		text = credential.pattern.ReplaceAllString(text, credential.replacement)
	}
	return text
}

// RedactBoundaryText returns text with detected personal data and credentials
// replaced by placeholders, under the model and tool boundary policy.
//
// It is deterministic and entirely in process: text with nothing to find
// comes back unchanged.
func (p *Policy) RedactBoundaryText(ctx context.Context, text string) string {
	return p.redactText(text, boundaryPolicy())
}

// RedactPersistedText redacts free text before it reaches durable local state.
func (p *Policy) RedactPersistedText(ctx context.Context, text string) string {
	return p.redactText(text, persistedPolicy())
}

// PersistedRedactor adapts [Policy.RedactPersistedText] to the context-free
// shape the tools package's Redactor seam expects.
//
// The audit write path is reached through ADK's tool machinery, which hands a
// tool its own context but not the redactor, so the context is bound once here.
func (p *Policy) PersistedRedactor(ctx context.Context) func(string) string {
	return func(text string) string { return p.RedactPersistedText(ctx, text) }
}

// redactText runs the tripwires, then the analysis, then the masking.
func (p *Policy) redactText(text string, policy redactionPolicy) string {
	if policy.blankPassesThrough && strings.TrimSpace(text) == "" {
		return text
	}
	redacted := redactCredentials(text)
	spans := p.analyze(redacted, policy)
	if len(spans) == 0 {
		return redacted
	}
	return maskSpans(redacted, spans)
}

// analyze detects the configured entities without corrupting exact operational
// evidence, in three phases.
//
//  1. Containers first. An email address can contain a token that would
//     otherwise be protected, so container spans are found on the raw text and
//     win over the protection below.
//  2. Protect. Every domain-safe token outside a container is overwritten in a
//     working copy with same-length underscores, so downstream offsets stay
//     valid. Underscores rather than spaces: whitespace lets an NER model
//     extend a person span across the protected position.
//  3. Analyze the rest, on the masked copy, and drop anything that overlaps a
//     protected or a container span.
func (p *Policy) analyze(text string, policy redactionPolicy) []span {
	var containers []span
	if policy.wants(entityEmail) {
		containers = matchRecognizers(text, func(name entity) bool { return name == entityEmail })
	}

	protected := protectedSpans(text, containers)
	masked := maskProtected(text, protected)

	residual := matchRecognizers(masked, func(name entity) bool {
		return name != entityEmail && policy.wants(name)
	})
	detected := make([]span, 0, len(containers)+len(residual))
	detected = append(detected, containers...)
	for _, candidate := range residual {
		if candidate.overlapsAny(protected) || candidate.overlapsAny(containers) {
			continue
		}
		detected = append(detected, candidate)
	}
	return detected
}

// matchRecognizers runs every deterministic recognizer the predicate admits.
func matchRecognizers(text string, wanted func(entity) bool) []span {
	var found []span
	for _, detector := range recognizers() {
		if !wanted(detector.entity) {
			continue
		}
		for _, match := range detector.pattern.FindAllStringIndex(text, -1) {
			if detector.validate != nil && !detector.validate(text[match[0]:match[1]]) {
				continue
			}
			found = append(found, span{entity: detector.entity, start: match[0], end: match[1]})
		}
	}
	return found
}

// protectedSpans returns the domain-safe token ranges that do not sit inside a
// container span. A safe token inside an email address is part of the address,
// not evidence, so protecting it would leave the address readable.
func protectedSpans(text string, containers []span) []span {
	var protected []span
	for _, match := range boundedMatches(domainSafeToken, text) {
		if match.overlapsAny(containers) {
			continue
		}
		protected = append(protected, match)
	}
	return protected
}

// maskProtected overwrites every protected range with same-length underscores.
// Domain-safe tokens are ASCII, so byte length is preserved and every offset
// computed on the result is still valid on the original.
func maskProtected(text string, protected []span) string {
	if len(protected) == 0 {
		return text
	}
	masked := []byte(text)
	for _, match := range protected {
		for index := match.start; index < match.end; index++ {
			masked[index] = '_'
		}
	}
	return string(masked)
}

// boundedMatches returns every match of pattern that is not glued to a
// surrounding identifier character.
//
// The rule is the one a lookaround pair — (?<![\w-]) and (?![\w-]) — would
// express, and RE2 has no lookaround, so it is checked here instead. A rejected
// candidate advances the scan by one byte rather than past its end, so an
// overlapping candidate starting one position later is still reachable.
//
// One limit is inherent to RE2 and documented rather than hidden: a
// backtracking engine, on a failing trailing boundary, can retreat into the
// alternation and settle for a shorter token that does satisfy the boundary,
// and RE2 cannot. A pathological input such as a doubly-suffixed version tag is
// therefore left unprotected here. The bias is one-way — this protects fewer
// tokens than a backtracking engine would, never more, so the only reachable
// effect is an extra redaction, never a missed one — and no layer-1 recognizer
// matches a version string, so the difference is not observable in what gets
// redacted.
func boundedMatches(pattern *regexp.Regexp, text string) []span {
	var found []span
	for offset := 0; offset < len(text); {
		match := pattern.FindStringIndex(text[offset:])
		if match == nil {
			break
		}
		start, end := offset+match[0], offset+match[1]
		if isIdentifierByte(text, start-1) || isIdentifierByte(text, end) {
			offset = start + 1
			continue
		}
		found = append(found, span{start: start, end: end})
		offset = end
	}
	return found
}

// isIdentifierByte reports whether the byte at index continues an identifier.
// The reading is ASCII rather than Unicode-aware on purpose: every token
// domainSafeToken can produce is written in ASCII, so widening it would buy
// nothing and would cost a rune decode on every boundary test.
func isIdentifierByte(text string, index int) bool {
	if index < 0 || index >= len(text) {
		return false
	}
	character := text[index]
	return character == '-' || character == '_' ||
		(character >= '0' && character <= '9') ||
		(character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z')
}

// maskSpans replaces every detected span with its entity placeholder.
//
// Overlapping detections are resolved before rewriting, because two
// placeholders cannot occupy the same characters: earliest wins, then longest,
// then the entity name, so the same input always produces the same output.
func maskSpans(text string, spans []span) string {
	ordered := slices.Clone(spans)
	slices.SortFunc(ordered, func(left, right span) int {
		if left.start != right.start {
			return left.start - right.start
		}
		if left.end != right.end {
			return right.end - left.end
		}
		return strings.Compare(string(left.entity), string(right.entity))
	})

	var builder strings.Builder
	written := 0
	for _, detected := range ordered {
		if detected.start < written {
			continue
		}
		builder.WriteString(text[written:detected.start])
		builder.WriteString(detected.entity.mask())
		written = detected.end
	}
	builder.WriteString(text[written:])
	return builder.String()
}

// passesLuhn validates a candidate card number, ignoring its separators.
func passesLuhn(candidate string) bool {
	digits := make([]int, 0, len(candidate))
	for _, character := range candidate {
		if character >= '0' && character <= '9' {
			digits = append(digits, int(character-'0'))
		}
	}
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	return luhnValid(digits)
}

// passesCASINChecksum validates the nine digits of a Canadian SIN with Luhn.
func passesCASINChecksum(candidate string) bool {
	digits := decimalDigits(candidate)
	return len(digits) == 9 && luhnValid(digits)
}

// decimalDigits extracts ASCII digits from a formatted identifier.
func decimalDigits(candidate string) []int {
	digits := make([]int, 0, len(candidate))
	for _, character := range candidate {
		if character >= '0' && character <= '9' {
			digits = append(digits, int(character-'0'))
		}
	}
	return digits
}

// luhnValid applies the checksum after a recognizer has established the
// identifier's own length and shape.
func luhnValid(digits []int) bool {
	sum, double := 0, false
	for index := len(digits) - 1; index >= 0; index-- {
		value := digits[index]
		if double {
			if value *= 2; value > 9 {
				value -= 9
			}
		}
		sum += value
		double = !double
	}
	return sum%10 == 0
}

// isValidSSN rejects the U.S. Social Security Administration's structurally
// impossible groups. It does not claim issuance or identity verification.
func isValidSSN(candidate string) bool {
	digits := decimalDigits(candidate)
	if len(digits) != 9 {
		return false
	}
	area := digits[0]*100 + digits[1]*10 + digits[2]
	group := digits[3]*10 + digits[4]
	serial := digits[5]*1000 + digits[6]*100 + digits[7]*10 + digits[8]
	return area != 0 && area != 666 && area < 900 && group != 0 && serial != 0
}

// passesIBANChecksum validates a candidate with the ISO 13616 mod-97 rule: move
// the first four characters to the end, expand letters to two-digit numbers,
// and require a remainder of one.
func passesIBANChecksum(candidate string) bool {
	if len(candidate) < 15 || len(candidate) > 34 {
		return false
	}
	rearranged := candidate[4:] + candidate[:4]
	remainder := 0
	for _, character := range rearranged {
		switch {
		case character >= '0' && character <= '9':
			remainder = remainder*10 + int(character-'0')
		case character >= 'A' && character <= 'Z':
			remainder = remainder*100 + int(character-'A') + 10
		default:
			return false
		}
		remainder %= 97
	}
	return remainder == 1
}

// isIPAddress is the real gate behind both address patterns: a colon-separated
// hex run is only an address if the standard library can parse it.
func isIPAddress(candidate string) bool {
	address, err := netip.ParseAddr(candidate)
	return err == nil && address.IsValid()
}

// RedactBoundaryValue recursively redacts strings inside tool arguments and
// structured results under the boundary policy.
func (p *Policy) RedactBoundaryValue(ctx context.Context, value any) any {
	return p.redactValue(value, boundaryPolicy())
}

// RedactPersistedValue recursively redacts strings under the persisted policy.
func (p *Policy) RedactPersistedValue(ctx context.Context, value any) any {
	return p.redactValue(value, persistedPolicy())
}

// redactValue walks a structured value and redacts every string in it.
//
// The shapes handled are the ones that actually cross this boundary: ADK
// round-trips a tool result through JSON, so a result is a map of strings,
// numbers, booleans, nils, slices and maps. Typed string maps and slices are
// handled too because Go-native callers build them directly. Anything else — a
// number, a bool — is returned unchanged, because it holds no text to redact.
func (p *Policy) redactValue(value any, policy redactionPolicy) any {
	switch typed := value.(type) {
	case string:
		return p.redactText(typed, policy)
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[key] = p.redactValue(item, policy)
		}
		return redacted
	case map[string]string:
		redacted := make(map[string]string, len(typed))
		for key, item := range typed {
			redacted[key] = p.redactText(item, policy)
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = p.redactValue(item, policy)
		}
		return redacted
	case []string:
		redacted := make([]string, len(typed))
		for index, item := range typed {
			redacted[index] = p.redactText(item, policy)
		}
		return redacted
	default:
		return value
	}
}

// RedactRequest is the before-model guard: it masks personal data in every part
// of the outbound request before it leaves the process.
//
// It mutates the request in place and always returns nil, which is what lets
// the now-redacted request proceed to the model. Telemetry content capture is
// controlled separately, and session ingestion happens earlier still.
func (p *Policy) RedactRequest(ctx agent.Context, request *model.LLMRequest) (*model.LLMResponse, error) {
	if request == nil {
		return nil, nil
	}
	for _, content := range request.Contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" {
				part.Text = p.RedactBoundaryText(ctx, part.Text)
			}
			if part.FunctionCall != nil && len(part.FunctionCall.Args) > 0 {
				part.FunctionCall.Args = p.redactMap(ctx, part.FunctionCall.Args)
			}
			if part.FunctionResponse != nil && len(part.FunctionResponse.Response) > 0 {
				part.FunctionResponse.Response = p.redactMap(ctx, part.FunctionResponse.Response)
			}
		}
	}
	return nil, nil
}

// RedactResponse is the after-model guard: it redacts model text and the
// arguments of any tool call the model asked for, before either is used.
//
// It returns the same response when something changed and nil when nothing did,
// which is ADK's "unmodified" signal. Streaming partials pass through here too,
// one chunk at a time — an entity split across two chunks is only caught in the
// aggregate, and a fragment already streamed to a client cannot be retracted,
// which is one reason streaming is off by default.
func (p *Policy) RedactResponse(
	ctx agent.Context, response *model.LLMResponse, responseErr error,
) (*model.LLMResponse, error) {
	if response == nil || responseErr != nil || response.Content == nil {
		return nil, nil
	}
	changed := false
	for _, part := range response.Content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			redacted := p.RedactBoundaryText(ctx, part.Text)
			changed = changed || redacted != part.Text
			part.Text = redacted
		}
		if part.FunctionCall != nil && len(part.FunctionCall.Args) > 0 {
			redacted := p.redactMap(ctx, part.FunctionCall.Args)
			changed = changed || !reflect.DeepEqual(redacted, part.FunctionCall.Args)
			part.FunctionCall.Args = redacted
		}
	}
	if !changed {
		return nil, nil
	}
	return response, nil
}

// redactMap redacts a string-keyed map under the boundary policy, keeping the
// map type so a caller that needs one does not have to assert.
func (p *Policy) redactMap(ctx context.Context, values map[string]any) map[string]any {
	redacted, ok := p.RedactBoundaryValue(ctx, values).(map[string]any)
	if !ok {
		// Unreachable: redactValue returns a map for a map. Keeping the original
		// rather than panicking means a future change to the recursion shows up
		// as an unredacted value a test catches, not as a dead turn.
		return values
	}
	return redacted
}
