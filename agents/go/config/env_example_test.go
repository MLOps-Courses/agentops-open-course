package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnvExampleChangesWithTheConfigContract(t *testing.T) {
	type original struct {
		Value string `env:"EXAMPLE_VALUE" envDefault:"first"`
	}
	type changedDefault struct {
		Value string `env:"EXAMPLE_VALUE" envDefault:"second"`
	}
	type changedName struct {
		Value string `env:"RENAMED_VALUE" envDefault:"first"`
	}
	const source = "# {{ contract }}\n{{ setting \"Value\" false }}\n"

	originalOutput, err := renderEnvExampleTemplate(reflect.TypeFor[original](), source)
	if err != nil {
		t.Fatalf("rendering original contract: %v", err)
	}
	defaultOutput, err := renderEnvExampleTemplate(reflect.TypeFor[changedDefault](), source)
	if err != nil {
		t.Fatalf("rendering changed default: %v", err)
	}
	nameOutput, err := renderEnvExampleTemplate(reflect.TypeFor[changedName](), source)
	if err != nil {
		t.Fatalf("rendering changed name: %v", err)
	}

	if string(originalOutput) == string(defaultOutput) {
		t.Fatal("changing an envDefault did not change the generated example")
	}
	if string(originalOutput) == string(nameOutput) {
		t.Fatal("changing an env tag did not change the generated example")
	}
	if !strings.Contains(string(originalOutput), "# EXAMPLE_VALUE=first") {
		t.Fatalf("original output does not carry the reflected default:\n%s", originalOutput)
	}
	if !strings.Contains(string(nameOutput), "# RENAMED_VALUE=first") {
		t.Fatalf("renamed output does not carry the reflected variable:\n%s", nameOutput)
	}

	// A deliberate learner example can differ from the runtime default. The
	// contract digest still changes, so that exception cannot hide default drift.
	const exampleSource = "# {{ contract }}\n{{ example \"Value\" \"chosen\" false }}\n"
	originalExample, err := renderEnvExampleTemplate(reflect.TypeFor[original](), exampleSource)
	if err != nil {
		t.Fatalf("rendering original explicit example: %v", err)
	}
	changedExample, err := renderEnvExampleTemplate(reflect.TypeFor[changedDefault](), exampleSource)
	if err != nil {
		t.Fatalf("rendering changed explicit example: %v", err)
	}
	if string(originalExample) == string(changedExample) {
		t.Fatal("changing a default hidden behind an explicit example did not change the contract digest")
	}
}

func TestEnvExampleIsCompleteStableAndCurrent(t *testing.T) {
	first, err := RenderEnvExample()
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	second, err := RenderEnvExample()
	if err != nil {
		t.Fatalf("second render: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two renders from the same Config contract differ")
	}
	if len(first) == 0 || first[len(first)-1] != '\n' {
		t.Fatal("generated example must end with exactly one stable newline")
	}
	if err := CheckEnvExample(filepath.Join("..", "..", "..", ".env.example")); err != nil {
		t.Fatal(err)
	}
}

func TestEnvExampleWriteIsIdempotentAndStalenessIsDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env.example")
	if err := WriteEnvExample(path); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading first write: %v", err)
	}
	if writeErr := WriteEnvExample(path); writeErr != nil {
		t.Fatalf("second write: %v", writeErr)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading second write: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("writing an unchanged Config contract changed the file")
	}

	if err := os.WriteFile(path, append(second, []byte("# stale\n")...), 0o600); err != nil {
		t.Fatalf("making the fixture stale: %v", err)
	}
	if err := CheckEnvExample(path); err == nil || !strings.Contains(err.Error(), "mise run config:generate") {
		t.Fatalf("CheckEnvExample() error = %v, want regeneration guidance", err)
	}
}

func TestEnvExampleOmitsDeprecatedAliasesAndKeepsSecretsSafe(t *testing.T) {
	output, err := RenderEnvExample()
	if err != nil {
		t.Fatalf("rendering example: %v", err)
	}
	text := string(output)
	if strings.Contains(text, EnvGatewayEnabled) {
		t.Fatalf("deprecated %s appears in the generated example", EnvGatewayEnabled)
	}
	for _, safe := range []string{
		"OPENAI_API_KEY=local-ollama",
		"# GOOGLE_API_KEY=",
		"# AGENT_MCP_TOKEN=",
	} {
		if !strings.Contains(text, safe) {
			t.Errorf("generated example does not contain safe credential form %q", safe)
		}
	}
	for _, unsafe := range []string{"sk-", "AIza", "-----BEGIN", "your-real"} {
		if strings.Contains(text, unsafe) {
			t.Errorf("generated example contains unsafe credential marker %q", unsafe)
		}
	}

	type unsafeSecret struct {
		APIKey Secret `env:"API_KEY" envDefault:"sk-live-value"`
	}
	if _, err := renderEnvExampleTemplate(
		reflect.TypeFor[unsafeSecret](),
		"{{ setting \"APIKey\" false }}\n",
	); err == nil {
		t.Fatal("unsafe secret default was rendered without an error")
	}
}

func TestEnvExampleTemplateCannotHandWriteConfigAssignments(t *testing.T) {
	type fixture struct {
		Value string `env:"EXAMPLE_VALUE" envDefault:"safe"`
	}
	_, err := renderEnvExampleTemplate(
		reflect.TypeFor[fixture](),
		"EXAMPLE_VALUE=hand-edited\n{{ setting \"Value\" false }}\n",
	)
	if err == nil {
		t.Fatal("raw assignment in the checked template was accepted")
	}
}

func TestEnvExampleTemplateCannotRenderUncheckedAssignments(t *testing.T) {
	type fixture struct {
		APIKey Secret `env:"OPENAI_API_KEY"`
	}
	_, err := renderEnvExampleTemplate(
		reflect.TypeFor[fixture](),
		"# {{ contract }}\n{{ setting \"APIKey\" false }}\n{{ \"OPENAI_API_KEY=live-token\" }}\n",
	)
	if err == nil {
		t.Fatal("assignment emitted by an unchecked template action was accepted")
	}
}

func TestEnvExampleRejectsSemanticEnvTagOptions(t *testing.T) {
	type fixture struct {
		Value string `env:"EXAMPLE_VALUE,required" envDefault:"safe"`
	}
	_, err := renderEnvExampleTemplate(
		reflect.TypeFor[fixture](),
		"# {{ contract }}\n{{ setting \"Value\" false }}\n",
	)
	if err == nil {
		t.Fatal("semantic env tag options were discarded from the contract")
	}
}
