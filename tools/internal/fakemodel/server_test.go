package fakemodel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestResponsesContract(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, ResponsesPath,
		strings.NewReader(`{"model":"qwen3:4b-instruct","stream":false}`))
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if document["status"] != "completed" || document["model"] != "qwen3:4b-instruct" {
		t.Fatalf("response = %#v", document)
	}
	outputs, ok := document["output"].([]any)
	if !ok || len(outputs) != 1 {
		t.Fatalf("output = %#v", document["output"])
	}
	output, ok := outputs[0].(map[string]any)
	if !ok {
		t.Fatalf("output item = %#v", outputs[0])
	}
	contents, ok := output["content"].([]any)
	if !ok || len(contents) != 1 {
		t.Fatalf("content = %#v", output["content"])
	}
	content, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("content item = %#v", contents[0])
	}
	if content["text"] != ReplyText {
		t.Errorf("text = %q, want %q", content["text"], ReplyText)
	}
}

// The evaluation judge reads choices[0].message.content and fails with "no verdict
// content" when that field is absent, which is exactly how the gateway's misrouted
// chat-completions path used to break. Pin the shape the judge parses.
func TestChatCompletionsContract(t *testing.T) {
	body := `{"model":"qwen3:4b-instruct","messages":[{"role":"user","content":"hello"}]}`
	request := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(body))
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var document struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if document.Object != "chat.completion" || document.Model != "qwen3:4b-instruct" {
		t.Fatalf("response = %#v", document)
	}
	if len(document.Choices) != 1 || document.Choices[0].Message.Content != ReplyText {
		t.Fatalf("choices = %#v", document.Choices)
	}
}

func TestChatCompletionsAnswersTheSameGuardrailProbes(t *testing.T) {
	body := `{"model":"qwen3:4b-instruct","messages":[{"role":"user","content":"` +
		ResponseMaskProbe + `"}]}`
	request := httptest.NewRequest(http.MethodPost, ChatCompletionsPath, strings.NewReader(body))
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), ResponsePIIReply) {
		t.Fatalf("probe reply = %d %s", response.Code, response.Body)
	}
}

func TestRejectsStreamingAndUnknownPaths(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   string
		status int
	}{
		{"streaming", ResponsesPath, `{"stream":true}`, http.StatusBadRequest},
		{"malformed", ResponsesPath, `{`, http.StatusBadRequest},
		{"chat streaming", ChatCompletionsPath, `{"stream":true}`, http.StatusBadRequest},
		{"chat malformed", ChatCompletionsPath, `{`, http.StatusBadRequest},
		// The fixture serves exactly the two OpenAI shapes the gateway routes and
		// nothing else, so a widened surface fails here.
		{"wrong path", "/v1/completions", `{}`, http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			response := httptest.NewRecorder()
			Handler(nil).ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, HealthPath, nil)
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
		t.Fatalf("health = %d %s", response.Code, response.Body)
	}
}

func TestModelsListsTheCourseModel(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, ModelsPath, nil)
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"id":"qwen3:4b-instruct"`) {
		t.Fatalf("models = %d %s", response.Code, response.Body)
	}
}

func TestGuardrailProbeResponses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"unmasked request", `request-mask-probe request-mask@example.test`, RawRequestPIIReply},
		{"masked request", `request-mask-probe <EMAIL>`, MaskedRequestPIIReply},
		{"response mask", ResponseMaskProbe, ResponsePIIReply},
		{"response reject", ResponseRejectProbe, ResponseRejectPIIReply},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body := strings.NewReader(`{"model":"qwen3:4b-instruct","input":` +
				strconv.Quote(test.input) + `,"stream":false}`)
			request := httptest.NewRequest(http.MethodPost, ResponsesPath, body)
			response := httptest.NewRecorder()
			Handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), test.want) {
				t.Fatalf("response = %d %s, want %q", response.Code, response.Body, test.want)
			}
		})
	}
}

func TestNamedEntityProbeReturnsOneEmptyResultPerInput(t *testing.T) {
	t.Parallel()
	body := `{
		"model":"qwen3:4b-instruct",
		"input":[{"role":"user","content":[{"type":"input_text","text":"{\"items\":[{\"index\":0,\"text\":\"hello\"},{\"index\":1,\"text\":\"world\"}]}"}]}],
		"stream":false,
		"text":{"format":{"type":"json_schema"}}
	}`
	request := httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(body))
	response := httptest.NewRecorder()
	Handler(nil).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	var document struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Output) != 1 || len(document.Output[0].Content) != 1 {
		t.Fatalf("NER response = %#v", document)
	}
	var result struct {
		Items []struct {
			Entities []any `json:"entities"`
			Index    int   `json:"index"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(document.Output[0].Content[0].Text), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].Index != 0 || result.Items[1].Index != 1 ||
		len(result.Items[0].Entities) != 0 || len(result.Items[1].Entities) != 0 {
		t.Fatalf("NER result = %#v", result)
	}
}

// scriptFixture is the committed trajectory script the eval:offline task drives.
func scriptFixture(t *testing.T) *Script {
	t.Helper()
	script, err := LoadScript(filepath.Join("..", "..", "testdata", "ops-script.json"))
	if err != nil {
		t.Fatalf("LoadScript() error = %v", err)
	}
	return script
}

// scriptedCall posts one request and returns the decoded output items.
func scriptedCall(t *testing.T, script *Script, input string) []map[string]any {
	t.Helper()
	body := `{"model":"agentops-fake","input":` + input + `}`
	recorder := httptest.NewRecorder()
	Handler(script).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var decoded struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return decoded.Output
}

// replayed builds the input array a client would send after `completed` tool
// results, which is the only state the stateless handler reads.
func replayed(question string, completed int) string {
	items := make([]string, 0, 1+completed)
	items = append(items, `{"type":"message","role":"user","content":`+strconv.Quote(question)+`}`)
	for index := range completed {
		items = append(items,
			`{"type":"function_call_output","call_id":"call-`+strconv.Itoa(index)+`","output":"{}"}`)
	}
	return "[" + strings.Join(items, ",") + "]"
}

func TestScriptWalksEveryRequiredCaseTrajectory(t *testing.T) {
	script := scriptFixture(t)
	// The four cases evals/mise.toml pins as --required-cases, with the exact
	// trajectories ops.evalset.json asserts.
	for _, expected := range []struct {
		question string
		tools    []string
	}{
		{"Investigate INC-002.", []string{"recall_incident_context", "get_incident"}},
		{"Load the reviewed remediation skill directly", []string{"load_skill"}},
		{
			"INC-002 has crash-looping inventory pods and HTTP 503 stock lookups.",
			[]string{"get_incident", "get_service_status", "search_service_logs", "get_runbook", "restart_service"},
		},
		{
			"Search latency for INC-005 has returned to baseline.",
			[]string{"get_incident", "get_service_status", "get_runbook", "resolve_incident"},
		},
	} {
		t.Run(expected.tools[len(expected.tools)-1], func(t *testing.T) {
			for step, wanted := range expected.tools {
				output := scriptedCall(t, script, replayed(expected.question, step))
				if len(output) != 1 || output[0]["type"] != "function_call" {
					t.Fatalf("step %d output = %#v, want one function_call", step, output)
				}
				if output[0]["name"] != wanted {
					t.Fatalf("step %d called %v, want %s", step, output[0]["name"], wanted)
				}
				if _, ok := output[0]["arguments"].(string); !ok {
					t.Fatalf("step %d arguments = %#v, want a JSON string", step, output[0]["arguments"])
				}
			}
			// One more result than there are steps ends the trajectory in prose.
			output := scriptedCall(t, script, replayed(expected.question, len(expected.tools)))
			if len(output) != 1 || output[0]["type"] != "message" {
				t.Fatalf("final output = %#v, want a message", output)
			}
		})
	}
}

func TestScriptFallsThroughForAnUnscriptedPrompt(t *testing.T) {
	output := scriptedCall(t, scriptFixture(t), replayed("Something nobody scripted.", 0))
	if len(output) != 1 || output[0]["type"] != "message" {
		t.Fatalf("output = %#v, want the unscripted reply", output)
	}
	content, ok := output[0]["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %#v", output[0]["content"])
	}
	first, ok := content[0].(map[string]any)
	if !ok || first["text"] != ReplyText {
		t.Fatalf("text = %#v, want %q", content[0], ReplyText)
	}
}

func TestScriptedHandlerStillRefusesStreaming(t *testing.T) {
	recorder := httptest.NewRecorder()
	body := `{"model":"agentops-fake","stream":true,"input":` + replayed("Investigate INC-002.", 0) + `}`
	Handler(scriptFixture(t)).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, ResponsesPath, strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "streaming is intentionally unsupported") {
		t.Fatalf("body = %s", recorder.Body.String())
	}
}

func TestLoadScriptRejectsAnIncompleteScript(t *testing.T) {
	for name, content := range map[string]string{
		"no cases":      `{"cases":[]}`,
		"no id":         `{"cases":[{"match":"x","answer":"y","steps":[]}]}`,
		"no match":      `{"cases":[{"id":"a","answer":"y","steps":[]}]}`,
		"no answer":     `{"cases":[{"id":"a","match":"x","steps":[]}]}`,
		"step no tool":  `{"cases":[{"id":"a","match":"x","answer":"y","steps":[{"arguments":{}}]}]}`,
		"unknown field": `{"cases":[{"id":"a","match":"x","answer":"y","nope":1}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "script.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadScript(path); err == nil {
				t.Fatal("LoadScript accepted an incomplete script")
			}
		})
	}
}

// TestScriptedToolsExistInTheAgentsToolset is the assertion that keeps this fixture
// honest: a script naming a tool the agent does not declare produces a green offline
// run that proves nothing.
func TestScriptedToolsExistInTheAgentsToolset(t *testing.T) {
	declared := map[string]bool{}
	// The toolset is spread across three packages: the incident tools and guarded
	// writes in tools/, retrieval and recall in memory/, and the skill loader in
	// compose/. Read all three rather than assuming one owns them.
	for _, source := range []string{
		filepath.Join("tools", "tools.go"),
		filepath.Join("memory", "memory.go"),
		filepath.Join("compose", "skills.go"),
	} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "..", "agents", "go", source))
		if err != nil {
			continue
		}
		for _, match := range regexp.MustCompile(`ToolName\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(raw), -1) {
			declared[match[1]] = true
		}
	}
	if len(declared) < 10 {
		t.Skipf("could not resolve the agent toolset from source (found %d)", len(declared))
	}
	for _, scriptCase := range scriptFixture(t).Cases {
		for _, step := range scriptCase.Steps {
			if !declared[step.Tool] {
				t.Errorf("case %q scripts %q, which agents/go does not declare", scriptCase.ID, step.Tool)
			}
		}
	}
}
