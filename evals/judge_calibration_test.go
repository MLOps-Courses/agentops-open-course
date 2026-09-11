package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayJudgeUsesGatewayAndStrictVerdict(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer marker" {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		var payload struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "judge" || !strings.Contains(payload.Messages[0].Content, "untrusted data") {
			t.Fatalf("unsafe judge request: %+v", payload)
		}
		writeTestJSON(t, writer, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": `{"passed":true,"rationale":"grounded"}`}}},
		})
	}))
	defer server.Close()
	judge, err := NewGatewayJudge(GatewayJudgeConfig{
		BaseURL: server.URL + "/v1", Model: "judge", APIKey: "marker",
	})
	if err != nil {
		t.Fatal(err)
	}
	verdict, err := judge.Judge(context.Background(), JudgeInput{
		Questions: []string{"q"}, Answers: []string{"a"}, ReferenceAnswers: []string{"r"},
	})
	if err != nil || !verdict.Passed || verdict.Rationale != "grounded" {
		t.Fatalf("verdict/error = %+v/%v", verdict, err)
	}
}

func TestJudgeVerdictRejectsUnknownOrMissingFields(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		`{"passed":true,"rationale":"ok","score":1}`,
		`{"rationale":"ok"}`,
		`{"passed":true,"rationale":""}`,
		`{"passed":true,"rationale":"ok"} {}`,
	} {
		if _, err := parseJudgeVerdict(content); err == nil {
			t.Fatalf("invalid verdict accepted: %s", content)
		}
	}
}

func TestJudgeVerdictErrorsOmitProviderControlledFields(t *testing.T) {
	t.Parallel()

	const marker = "SYNTHETIC_PROVIDER_FIELD_DO_NOT_LOG"
	for name, content := range map[string]string{
		"unknown field":  `{"passed":true,"rationale":"ok","` + marker + `":true}`,
		"malformed tail": `{"passed":true,"rationale":"ok"} @` + marker,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseJudgeVerdict(content)
			if err == nil {
				t.Fatal("parseJudgeVerdict() error = nil, want invalid provider JSON to fail")
			}
			if got, want := err.Error(), "decode judge verdict"; got != want {
				t.Fatalf("parseJudgeVerdict() error = %q, want fixed local class %q", got, want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("parseJudgeVerdict() error retained a provider-controlled field")
			}
		})
	}
}

func TestCalibrationMeasuresAgreementAndStaysSanitized(t *testing.T) {
	t.Parallel()
	set, err := LoadCalibrationSet("judge-calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	result, err := Calibrate(
		context.Background(), set, fixedJudge{pass: true},
		ModelEvidence{Provider: "openai-compatible", Name: "judge", Digest: strings.Repeat("a", 64)},
		testSourceEvidence(),
	)
	if err != nil {
		t.Fatal(err)
	}
	// A judge that passes everything agrees with exactly the four good answers, and
	// its eight disagreements are all false passes. The 8/4 label split means an
	// always-reject judge would score 0.667, well above this judge's 0.333.
	if result.Matches != 4 || result.Total != 12 || result.Agreement != 4.0/12.0 {
		t.Fatalf("result = %+v", result)
	}
	if result.FalsePass != 8 || result.FalseFail != 0 || result.MajorityBaseline != 8.0/12.0 {
		t.Fatalf("error split = %+v", result)
	}
	if result.SchemaVersion != 5 || result.JudgeModel.Provider != "openai-compatible" ||
		result.JudgeModel.Digest != strings.Repeat("a", 64) {
		t.Fatalf("judge identity = %#v", result.JudgeModel)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"question", "reference_answer", "answer", "rationale"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("calibration artifact leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestCalibrationRequiresAJudgeIdentity(t *testing.T) {
	t.Parallel()
	set, err := LoadCalibrationSet("judge-calibration.json")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Calibrate(
		context.Background(), set, fixedJudge{pass: true},
		ModelEvidence{Provider: "openai-compatible"}, testSourceEvidence(),
	)
	if err == nil || !strings.Contains(err.Error(), "model name") {
		t.Fatalf("anonymous judge error = %v", err)
	}
}

func TestGatewayJudgeRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	valid := GatewayJudgeConfig{BaseURL: "http://gateway.test/v1", Model: "judge", APIKey: "marker"}
	for name, test := range map[string]struct {
		mutate func(*GatewayJudgeConfig)
		want   string
	}{
		"base URL": {mutate: func(c *GatewayJudgeConfig) { c.BaseURL = "gateway.test" }, want: "judge base URL"},
		"model":    {mutate: func(c *GatewayJudgeConfig) { c.Model = "" }, want: "judge model is required"},
		"api key":  {mutate: func(c *GatewayJudgeConfig) { c.APIKey = "" }, want: "judge API key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			config := valid
			test.mutate(&config)
			if _, err := NewGatewayJudge(config); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("NewGatewayJudge() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestGatewayJudgeRefusesMisalignedInputAndUnusableCompletions(t *testing.T) {
	t.Parallel()

	// The judge grades answer i against question i and reference i. Ragged slices
	// would silently grade the wrong pair, so they never reach the gateway.
	judge, err := NewGatewayJudge(GatewayJudgeConfig{
		BaseURL: "http://gateway.test/v1", Model: "judge", APIKey: "marker",
	})
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]JudgeInput{
		"empty":            {},
		"missing answer":   {Questions: []string{"q"}, ReferenceAnswers: []string{"r"}},
		"extra reference":  {Questions: []string{"q"}, Answers: []string{"a"}, ReferenceAnswers: []string{"r", "r"}},
		"unequal question": {Questions: []string{"q", "q"}, Answers: []string{"a"}, ReferenceAnswers: []string{"r"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := judge.Judge(t.Context(), input); err == nil ||
				!strings.Contains(err.Error(), "equally sized") {
				t.Fatalf("Judge(%+v) error = %v, want a misaligned-input refusal", input, err)
			}
		})
	}

	for name, test := range map[string]struct {
		handler http.HandlerFunc
		want    string
	}{
		"gateway rejection": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				http.Error(writer, "quota exhausted for tenant", http.StatusTooManyRequests)
			},
			want: "HTTP 429",
		},
		"unreadable completion": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"choices":`))
			},
			want: "decode judge response",
		},
		"no verdict content": {
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writeTestJSON(t, writer, map[string]any{"choices": []any{}})
			},
			want: "no verdict content",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(test.handler)
			defer server.Close()
			gateway, gatewayErr := NewGatewayJudge(GatewayJudgeConfig{
				BaseURL: server.URL + "/v1", Model: "judge", APIKey: "marker",
			})
			if gatewayErr != nil {
				t.Fatal(gatewayErr)
			}
			_, err := gateway.Judge(t.Context(), JudgeInput{
				Questions: []string{"q"}, Answers: []string{"a"}, ReferenceAnswers: []string{"r"},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Judge() error = %v, want one mentioning %q", err, test.want)
			}
			if strings.Contains(fmt.Sprint(err), "quota exhausted for tenant") {
				t.Fatal("Judge() error retained the gateway response body")
			}
		})
	}
}

func TestLoadCalibrationSetRefusesUnusableFiles(t *testing.T) {
	t.Parallel()

	balanced, err := json.Marshal(balancedCalibrationSet())
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	for name, test := range map[string]struct {
		body string
		want string
	}{
		"unknown field":  {body: `{"schema_version":1,"cases":[],"reviewer":"nobody"}`, want: "decode judge calibration set"},
		"trailing value": {body: string(balanced) + " {}", want: "decode judge calibration set"},
		"unbalanced":     {body: `{"schema_version":1,"cases":[]}`, want: "validate judge calibration set"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".json")
			if writeErr := os.WriteFile(path, []byte(test.body), 0o600); writeErr != nil {
				t.Fatalf("WriteFile(calibration) error = %v", writeErr)
			}
			if _, loadErr := LoadCalibrationSet(path); loadErr == nil ||
				!strings.Contains(loadErr.Error(), test.want) {
				t.Fatalf("LoadCalibrationSet() error = %v, want one mentioning %q", loadErr, test.want)
			}
		})
	}
	if _, err := LoadCalibrationSet(filepath.Join(directory, "absent.json")); err == nil ||
		!strings.Contains(err.Error(), "read judge calibration set") {
		t.Fatalf("LoadCalibrationSet(absent) error = %v, want a read failure", err)
	}
}

func TestCalibrationSetValidateNamesEveryDefect(t *testing.T) {
	t.Parallel()

	if err := balancedCalibrationSet().Validate(); err != nil {
		t.Fatalf("Validate(balanced) error = %v", err)
	}
	for name, test := range map[string]struct {
		mutate func(*CalibrationSet)
		want   string
	}{
		"schema version": {mutate: func(s *CalibrationSet) { s.SchemaVersion = 2 }, want: "schema_version=2"},
		"too few cases": {
			mutate: func(s *CalibrationSet) { s.Cases = s.Cases[:3] },
			want:   "at least 12 labeled cases",
		},
		"missing id":   {mutate: func(s *CalibrationSet) { s.Cases[0].ID = "" }, want: "case 1 has no id"},
		"duplicate id": {mutate: func(s *CalibrationSet) { s.Cases[1].ID = s.Cases[0].ID }, want: "duplicate calibration id"},
		"unknown category": {
			mutate: func(s *CalibrationSet) { s.Cases[0].Category = "borderline" },
			want:   "invalid category",
		},
		"empty answer": {mutate: func(s *CalibrationSet) { s.Cases[0].Answer = "" }, want: "empty question, reference, or answer"},
		"imbalanced": {
			// An unbalanced set inflates agreement: a judge that always fails would
			// score well on a set that is mostly bad answers, and the number would
			// say nothing about whether the judge can recognize a good one.
			mutate: func(s *CalibrationSet) { s.Cases[0].Category = "bad" },
			want:   "must balance good, bad, and hallucinated",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			set := balancedCalibrationSet()
			test.mutate(&set)
			if err := set.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want one mentioning %q", err, test.want)
			}
		})
	}
}

func TestCalibrationRefusesAnUnidentifiedCheckoutAndSurfacesJudgeFailure(t *testing.T) {
	t.Parallel()

	set := balancedCalibrationSet()
	dirty := SourceEvidence{Revision: strings.Repeat("a", 40), Dirty: true}
	if _, err := Calibrate(t.Context(), set, fixedJudge{}, ModelEvidence{Provider: "p", Name: "m"}, dirty); err == nil ||
		!strings.Contains(err.Error(), "dirty checkout") {
		t.Fatalf("Calibrate(dirty source) error = %v, want a source refusal", err)
	}

	// A judge that fails mid-set produces a partial agreement number. Reporting
	// one would be worse than reporting none, so the whole measurement fails.
	const marker = "SYNTHETIC_JUDGE_TRANSPORT_DETAIL"
	failing := errorJudge{err: errors.New(marker)}
	_, err := Calibrate(t.Context(), set, failing, ModelEvidence{Provider: "p", Name: "m"}, testSourceEvidence())
	if err == nil || !strings.Contains(err.Error(), "judge calibration case") {
		t.Fatalf("Calibrate(failing judge) error = %v, want the case that failed", err)
	}
	if !strings.Contains(err.Error(), set.Cases[0].ID) {
		t.Fatalf("Calibrate(failing judge) error = %v, want it to name the first case", err)
	}
}

func balancedCalibrationSet() CalibrationSet {
	set := CalibrationSet{SchemaVersion: 1}
	for index := range 4 {
		for _, category := range []string{"good", "bad", "hallucinated"} {
			set.Cases = append(set.Cases, CalibrationCase{
				ID: fmt.Sprintf("%s-%d", category, index+1), Category: category,
				Question: "question", ReferenceAnswer: "reference", Answer: "answer",
				ExpectedPass: category == "good",
			})
		}
	}
	return set
}

type errorJudge struct{ err error }

func (judge errorJudge) Judge(context.Context, JudgeInput) (JudgeVerdict, error) {
	return JudgeVerdict{}, judge.err
}

type fixedJudge struct{ pass bool }

func (judge fixedJudge) Judge(context.Context, JudgeInput) (JudgeVerdict, error) {
	return JudgeVerdict{Passed: judge.pass, Rationale: "not serialized"}, nil
}
