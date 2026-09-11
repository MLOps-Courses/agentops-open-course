package config

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"text/template"
)

const envExampleUsage = "config:example [--check PATH | --write PATH]"

// envExampleTemplate keeps learner guidance reviewable while the renderer owns
// names and defaults. Template assignment lines must use one of the checked
// helpers below; a raw NAME=value line is rejected.
//
//go:embed env.example.tmpl
var envExampleTemplate string

var rawEnvAssignment = regexp.MustCompile(`(?m)^[ \t]*#?[ \t]*[A-Z][A-Z0-9_]*=.*$`)

// externalExampleVariables are read by dependencies rather than Config. They
// stay explicit so the template cannot silently grow an unrelated environment
// surface.
var externalExampleVariables = map[string]struct{}{
	"ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS": {},
	"AGENTOPS_GATEWAY_NETWORK":             {},
	"ALERTMANAGER_PORT":                    {},
	"EVAL_JUDGE_API_KEY":                   {},
	"EVAL_JUDGE_BASE_URL":                  {},
	"EVAL_JUDGE_MODEL":                     {},
	"EVAL_JUDGE_TIMEOUT_S":                 {},
	"EVAL_TURN_TIMEOUT_S":                  {},
	"GRAFANA_PORT":                         {},
	"LOKI_PORT":                            {},
	"OTEL_EXPORTER_OTLP_ENDPOINT":          {},
	"OTEL_EXPORTER_OTLP_PROTOCOL":          {},
	"OTEL_GRPC_PORT":                       {},
	"OTEL_HTTP_PORT":                       {},
	"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT": {},
	"OTEL_SERVICE_NAME": {},
	"PROMETHEUS_PORT":   {},
	"SSL_CERT_FILE":     {},
	"TEMPO_PORT":        {},
}

type envExampleField struct {
	goName       string
	variable     string
	defaultValue string
	typeName     string
	hasDefault   bool
	secret       bool
}

// RenderEnvExample derives the learner-facing example from Config's env tags
// and defaults, then applies the checked prose and example-value template.
func RenderEnvExample() ([]byte, error) {
	return renderEnvExampleTemplate(reflect.TypeFor[Config](), envExampleTemplate)
}

// EnvExampleCommand exposes generation and exact-diff checking without adding
// a second repository tool or parsing Config source text.
func EnvExampleCommand(arguments []string, out io.Writer) error {
	switch {
	case len(arguments) == 0:
		generated, err := RenderEnvExample()
		if err != nil {
			return err
		}
		if _, err := out.Write(generated); err != nil {
			return fmt.Errorf("writing generated environment example: %w", err)
		}
		return nil
	case len(arguments) == 2 && arguments[0] == "--check":
		return CheckEnvExample(arguments[1])
	case len(arguments) == 2 && arguments[0] == "--write":
		return WriteEnvExample(arguments[1])
	default:
		return fmt.Errorf("usage: %s", envExampleUsage)
	}
}

// WriteEnvExample regenerates the repository file. Rendering and validation
// finish before the existing file is opened, so an invalid template cannot
// truncate a valid example.
func WriteEnvExample(path string) error {
	generated, err := RenderEnvExample()
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, generated, 0o644); err != nil {
		return fmt.Errorf("writing generated environment example %q: %w", path, err)
	}
	return nil
}

// CheckEnvExample fails when the committed example is not the exact output of
// the current Config contract and checked template.
func CheckEnvExample(path string) error {
	generated, err := RenderEnvExample()
	if err != nil {
		return err
	}
	committed, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading environment example %q: %w", path, err)
	}
	if !bytes.Equal(committed, generated) {
		return fmt.Errorf("%s is stale; run `mise run config:generate` from the repository root", path)
	}
	return nil
}

func renderEnvExampleTemplate(configType reflect.Type, source string) ([]byte, error) {
	if configType.Kind() != reflect.Struct {
		return nil, fmt.Errorf("environment example contract must be a struct, got %s", configType)
	}
	if rawEnvAssignment.MatchString(source) {
		return nil, errors.New("environment example template contains a raw assignment; use a checked template helper")
	}

	fields, err := envExampleFields(configType)
	if err != nil {
		return nil, err
	}
	byName := make(map[string]envExampleField, len(fields))
	for _, field := range fields {
		byName[field.goName] = field
	}

	primaryUses := make(map[string]int, len(fields))
	emittedAssignments := make(map[string]int, len(fields))
	contractUses := 0
	lookup := func(goName string) (envExampleField, error) {
		field, ok := byName[goName]
		if !ok {
			return envExampleField{}, fmt.Errorf("environment example references unknown Config field %q", goName)
		}
		if field.variable == EnvGatewayEnabled {
			return envExampleField{}, fmt.Errorf("deprecated variable %s must stay absent from the environment example", field.variable)
		}
		return field, nil
	}
	assignment := func(field envExampleField, value string, active bool) (string, error) {
		if strings.ContainsAny(value, "\r\n") {
			return "", fmt.Errorf("example value for %s contains a newline", field.variable)
		}
		if field.secret && value != "" && value != "local-ollama" {
			return "", fmt.Errorf("example value for secret %s must be empty or the local-ollama marker", field.variable)
		}
		prefix := "# "
		if active {
			prefix = ""
		}
		line := prefix + field.variable + "=" + value
		emittedAssignments[line]++
		return line, nil
	}
	setting := func(goName string, active bool) (string, error) {
		field, lookupErr := lookup(goName)
		if lookupErr != nil {
			return "", lookupErr
		}
		primaryUses[goName]++
		return assignment(field, field.defaultValue, active)
	}
	example := func(goName, value string, active bool) (string, error) {
		field, lookupErr := lookup(goName)
		if lookupErr != nil {
			return "", lookupErr
		}
		primaryUses[goName]++
		return assignment(field, value, active)
	}
	alternate := func(goName, value string, active bool) (string, error) {
		field, lookupErr := lookup(goName)
		if lookupErr != nil {
			return "", lookupErr
		}
		return assignment(field, value, active)
	}
	external := func(variable, value string, active bool) (string, error) {
		if _, ok := externalExampleVariables[variable]; !ok {
			return "", fmt.Errorf("environment example references unapproved external variable %q", variable)
		}
		return assignment(envExampleField{variable: variable}, value, active)
	}
	contract := func() string {
		contractUses++
		hash := sha256.New()
		for _, field := range fields {
			_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%s\x00%t\x00%s\n",
				field.goName, field.variable, field.defaultValue, field.hasDefault, field.typeName)
		}
		return fmt.Sprintf("sha256:%x", hash.Sum(nil))
	}

	parsed, err := template.New("env.example").Option("missingkey=error").Funcs(template.FuncMap{
		"alternate": alternate,
		"contract":  contract,
		"example":   example,
		"external":  external,
		"setting":   setting,
	}).Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parsing environment example template: %w", err)
	}
	var output bytes.Buffer
	if err := parsed.Execute(&output, nil); err != nil {
		return nil, fmt.Errorf("rendering environment example template: %w", err)
	}
	renderedAssignments := make(map[string]int, len(emittedAssignments))
	for _, line := range rawEnvAssignment.FindAllString(output.String(), -1) {
		renderedAssignments[line]++
	}
	if !sameAssignmentCounts(emittedAssignments, renderedAssignments) {
		return nil, errors.New("environment example rendered an assignment that did not come from one checked template helper")
	}
	if contractUses != 1 {
		return nil, errors.New("environment example template must render the Config contract digest exactly once")
	}
	for _, field := range fields {
		if field.variable == EnvGatewayEnabled {
			if primaryUses[field.goName] != 0 {
				return nil, fmt.Errorf("deprecated variable %s is documented", field.variable)
			}
			continue
		}
		if primaryUses[field.goName] != 1 {
			return nil, fmt.Errorf("config.%s must have exactly one primary environment example, got %d", field.goName, primaryUses[field.goName])
		}
	}

	// Normalize only the final newline. Internal whitespace remains template
	// authority and therefore stays visible in the committed byte comparison.
	return []byte(strings.TrimRight(output.String(), "\n") + "\n"), nil
}

func sameAssignmentCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for assignment, count := range left {
		if right[assignment] != count {
			return false
		}
	}
	return true
}

func envExampleFields(configType reflect.Type) ([]envExampleField, error) {
	secretType := reflect.TypeFor[Secret]()
	fields := make([]envExampleField, 0, configType.NumField())
	for i := range configType.NumField() {
		field := configType.Field(i)
		variable, ok := field.Tag.Lookup("env")
		if !ok || variable == "" {
			return nil, fmt.Errorf("config.%s has no environment tag", field.Name)
		}
		variable, _, hasOptions := strings.Cut(variable, ",")
		if variable == "" {
			return nil, fmt.Errorf("config.%s has an empty environment variable name", field.Name)
		}
		if hasOptions {
			return nil, fmt.Errorf("config.%s uses unsupported environment tag options", field.Name)
		}
		defaultValue, hasDefault := field.Tag.Lookup("envDefault")
		baseType := field.Type
		if baseType.Kind() == reflect.Pointer {
			baseType = baseType.Elem()
		}
		fields = append(fields, envExampleField{
			goName:       field.Name,
			variable:     variable,
			defaultValue: defaultValue,
			typeName:     field.Type.String(),
			hasDefault:   hasDefault,
			secret:       baseType == secretType,
		})
	}
	return fields, nil
}
