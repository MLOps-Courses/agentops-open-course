package config

import (
	"bytes"
	"strings"
	"testing"
)

// runCheck exercises the config:check entrypoint against the process
// environment and returns its exit code with both streams.
func runCheck(t *testing.T) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Check(&out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCheckReportsValidConfiguration(t *testing.T) {
	cleanEnvironment(t)

	code, out, errOut := runCheck(t)
	if code != 0 {
		t.Fatalf("Check = %d, want 0\nstderr:\n%s", code, errOut)
	}
	assertContains(t, out,
		"Agent configuration is valid",
		"- AGENT_MODEL_PROVIDER = openai-compatible",
		"- AGENT_MODEL = qwen3:4b-instruct")
	if errOut != "" {
		t.Errorf("Check wrote to stderr on success:\n%s", errOut)
	}
}

func TestCheckReportsEverySettingSortedByVariable(t *testing.T) {
	cleanEnvironment(t)

	_, out, _ := runCheck(t)
	previous := ""
	printed := 0
	for _, line := range strings.Split(out, "\n") {
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), " = ")
		if !ok || !strings.HasPrefix(line, "- ") {
			continue
		}
		if name <= previous {
			t.Errorf("settings are not sorted: %q follows %q", name, previous)
		}
		previous = name
		printed++
	}
	// Every field is reported, including the deprecated alias, so an operator
	// can see exactly what the process resolved.
	if printed != len(specs()) {
		t.Errorf("printed %d settings, want %d", printed, len(specs()))
	}
}

func TestCheckMasksTheOpenAIKey(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvModelProvider, string(ProviderOpenAICompatible))
	t.Setenv(EnvOpenAIBaseURL, "http://127.0.0.1:4000/v1")
	t.Setenv(EnvOpenAIAPIKey, "super-sensitive")

	code, out, errOut := runCheck(t)
	if code != 0 {
		t.Fatalf("Check = %d, want 0\nstderr:\n%s", code, errOut)
	}
	assertContains(t, out, SecretMask, "- OPENAI_API_KEY = "+SecretMask)
	if strings.Contains(out, "super-sensitive") {
		t.Errorf("config:check printed the plaintext key:\n%s", out)
	}
}

func TestCheckMasksTheGeminiKey(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvModelProvider, string(ProviderGemini))
	t.Setenv(EnvGoogleAPIKey, "gemini-sensitive")

	code, out, errOut := runCheck(t)
	if code != 0 {
		t.Fatalf("Check = %d, want 0\nstderr:\n%s", code, errOut)
	}
	assertContains(t, out, "- GOOGLE_API_KEY = "+SecretMask)
	if strings.Contains(out, "gemini-sensitive") {
		t.Errorf("config:check printed the plaintext key:\n%s", out)
	}
}

func TestCheckFailsWithActionableErrors(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvModelProvider, string(ProviderOpenAICompatible))
	t.Setenv(EnvOpenAIBaseURL, "")
	t.Setenv(EnvOpenAIAPIKey, "")

	code, out, errOut := runCheck(t)
	if code != 1 {
		t.Fatalf("Check = %d, want 1\nstdout:\n%s", code, out)
	}
	assertContains(t, errOut, "Agent configuration is invalid:", "OPENAI_BASE_URL", "OPENAI_API_KEY")
	// Both problems are reported at once, not the first one twice.
	if strings.Count(errOut, "\n- ") != 2 {
		t.Errorf("want 2 bullet problems, got:\n%s", errOut)
	}
	if out != "" {
		t.Errorf("Check printed a resolved configuration despite failing:\n%s", out)
	}
}

func TestCheckNamesTheVariableForFieldProblems(t *testing.T) {
	cleanEnvironment(t)
	t.Setenv(EnvA2APort, "70000")

	code, _, errOut := runCheck(t)
	if code != 1 {
		t.Fatalf("Check = %d, want 1", code)
	}
	assertContains(t, errOut, "- "+EnvA2APort+": ")
}

func TestDescribeMarksUnsetOptionalSettings(t *testing.T) {
	cfg := mustLoad(t, validBase())

	byVariable := make(map[string]string, len(specs()))
	for _, setting := range cfg.Describe() {
		byVariable[setting.Variable] = setting.Value
	}
	// An empty line would read as "set to nothing", which for OPENAI_API_KEY is
	// a different state with a different outcome.
	for _, variable := range []string{
		EnvModelTemperature, EnvModelFallback, EnvMaxHistoryMessages,
		EnvMaxTokensPerSession, EnvTrustedIdentityHeader, EnvGatewayEnabled,
		EnvGoogleAPIKey, EnvMCPURL,
	} {
		if byVariable[variable] != UnsetValue {
			t.Errorf("%s = %q, want %q", variable, byVariable[variable], UnsetValue)
		}
	}
	if byVariable[EnvOpenAIAPIKey] != SecretMask {
		t.Errorf("%s = %q, want the mask", EnvOpenAIAPIKey, byVariable[EnvOpenAIAPIKey])
	}
	if byVariable[EnvSanitizeToolOutput] != "true" {
		t.Errorf("%s = %q, want true", EnvSanitizeToolOutput, byVariable[EnvSanitizeToolOutput])
	}
}

func TestDescribeRendersOptionalValuesRatherThanPointers(t *testing.T) {
	cfg := mustLoad(t, withEnv(map[string]string{
		EnvModelTemperature:   "0",
		EnvMaxHistoryMessages: "40",
		EnvModelFallback:      "qwen3:1.7b",
	}))

	byVariable := make(map[string]string, len(specs()))
	for _, setting := range cfg.Describe() {
		byVariable[setting.Variable] = setting.Value
	}
	for variable, want := range map[string]string{
		EnvModelTemperature:   "0",
		EnvMaxHistoryMessages: "40",
		EnvModelFallback:      "qwen3:1.7b",
	} {
		if byVariable[variable] != want {
			t.Errorf("%s = %q, want %q", variable, byVariable[variable], want)
		}
	}
}
