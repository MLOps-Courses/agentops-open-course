package evals

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

var identifierPattern = regexp.MustCompile(`(?i)\b(?:INC-\d+|SEV\d+)\b`)

var ambiguousServiceTerms = map[string]struct{}{
	"cache":  {},
	"search": {},
}

func ClaimedEntities(text string, domain Domain) []string {
	lowered := strings.ToLower(text)
	entities := make(map[string]struct{})
	for _, match := range identifierPattern.FindAllString(lowered, -1) {
		entities[match] = struct{}{}
	}
	for service := range domain.Services {
		if claimsService(lowered, service) {
			entities[service] = struct{}{}
		}
	}
	for runbook := range domain.Runbooks {
		if wordMatches(lowered, runbook) {
			entities[runbook] = struct{}{}
		}
	}
	result := make([]string, 0, len(entities))
	for entity := range entities {
		result = append(result, entity)
	}
	slices.Sort(result)
	return result
}

func UnsupportedClaims(turn Turn, question string, domain Domain) []string {
	grounding := question + " " + turn.Evidence()
	problems := make([]string, 0)
	for _, entity := range ClaimedEntities(turn.Text, domain) {
		_, ambiguous := ambiguousServiceTerms[entity]
		grounded := wordMatches(grounding, entity)
		if ambiguous {
			grounded = claimsService(grounding, entity)
		}
		if !grounded {
			problems = append(problems, fmt.Sprintf("answer claims %q with no supporting evidence", entity))
		}
	}
	return problems
}

func GroundednessScore(turn Turn, question string, domain Domain) Score {
	problems := UnsupportedClaims(turn, question, domain)
	if len(problems) > 0 {
		return NewBinaryScore("groundedness", false, strings.Join(problems, "; "))
	}
	return NewBinaryScore("groundedness", true, "every recognized entity appears in this turn's question or tool evidence")
}

func wordMatches(text, term string) bool {
	pattern := `(?i)(^|[^a-z0-9_-])` + regexp.QuoteMeta(term) + `([^a-z0-9_-]|$)`
	return regexp.MustCompile(pattern).MatchString(text)
}

func claimsService(text, term string) bool {
	if !wordMatches(text, term) {
		return false
	}
	if _, ambiguous := ambiguousServiceTerms[term]; !ambiguous {
		return true
	}
	escaped := regexp.QuoteMeta(term)
	state := `(?:up|down|healthy|unhealthy|degraded|unknown|operational|unavailable|failing|failed)`
	patterns := []string{
		`(?i)\b` + escaped + `\s+service\b`,
		`(?i)\bservice(?:\s+named)?\s+` + escaped + `\b`,
		`(?i)\b` + escaped + `\s+(?:is|was|remains|appears|looks)\s+(?:\w+\s+){0,2}` + state + `\b`,
		`(?i)\b` + escaped + `\s+(?:has|had|reports?)\s+(?:elevated\s+)?(?:errors?|latency|failures?|timeouts?)\b`,
		`(?i)["']service["']\s*:\s*["']` + escaped + `["']`,
		`(?i)["']name["']\s*:\s*["']` + escaped + `["']`,
	}
	for _, pattern := range patterns {
		if regexp.MustCompile(pattern).MatchString(text) {
			return true
		}
	}
	return false
}
