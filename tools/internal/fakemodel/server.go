package fakemodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	ResponsesPath = "/v1/responses"
	// The evaluation judge (evals/judge.go) speaks chat-completions, and the host
	// gateway routes that shape to the same governed model route as the agent's
	// Responses calls. The fixture stands in for Ollama in scripts/smoke-host.sh,
	// so it has to serve both endpoints or the smoke cannot prove that routing.
	ChatCompletionsPath = "/v1/chat/completions"
	ModelsPath          = "/v1/models"
	HealthPath          = "/healthz"
	ReplyText           = "Fake model response for platform latency measurement."

	RawRequestPIIReply     = "FAKE_MODEL_SAW_UNMASKED_REQUEST_PII"
	MaskedRequestPIIReply  = "FAKE_MODEL_SAW_MASKED_REQUEST_PII"
	ResponseMaskProbe      = "response-mask-probe"
	ResponsePIIReply       = "Contact response-mask@example.test"
	ResponseRejectProbe    = "response-reject-probe"
	ResponseRejectPIIReply = "Contact reject-probe@example.invalid"

	requestMaskProbe = "request-mask-probe"
	requestMaskPII   = "request-mask@example.test"
	defaultModel     = "agentops-fake"
)

type request struct {
	Model string `json:"model"`
	Text  struct {
		Format struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"text"`
	Input  json.RawMessage `json:"input"`
	Stream bool            `json:"stream"`
}

// chatRequest is the chat-completions counterpart of request. Only the fields the
// fixture reacts to are declared; everything else the judge sends is ignored.
type chatRequest struct {
	Model    string          `json:"model"`
	Messages json.RawMessage `json:"messages"`
	Stream   bool            `json:"stream"`
}

type errorEnvelope struct {
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Handler serves the fixture. A nil script keeps the original single-reply
// behavior, which is what scripts/smoke-host.sh drives.
func Handler(script *Script) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+HealthPath, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET "+ModelsPath, func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{map[string]string{"id": "qwen3:4b-instruct", "object": "model"}},
		})
	})
	mux.HandleFunc("POST "+ResponsesPath, func(writer http.ResponseWriter, incoming *http.Request) {
		var parsed request
		if !decodeSingleObject(writer, incoming, &parsed) {
			return
		}
		if parsed.Stream {
			writeBadRequest(writer, "streaming is intentionally unsupported; keep AGENT_A2A_STREAMING=false")
			return
		}
		if parsed.Model == "" {
			parsed.Model = defaultModel
		}
		if scriptCase := script.match(parsed.Input); scriptCase != nil && parsed.Text.Format.Type != "json_schema" {
			if output, more := scriptedOutput(scriptCase, completedSteps(parsed.Input)); more {
				writeJSON(writer, http.StatusOK, responseWithOutput(parsed.Model, output))
				return
			}
			writeJSON(writer, http.StatusOK, response(parsed.Model, scriptCase.Answer))
			return
		}
		writeJSON(writer, http.StatusOK, response(parsed.Model, responseText(parsed)))
	})
	mux.HandleFunc("POST "+ChatCompletionsPath, func(writer http.ResponseWriter, incoming *http.Request) {
		var parsed chatRequest
		if !decodeSingleObject(writer, incoming, &parsed) {
			return
		}
		if parsed.Stream {
			writeBadRequest(writer, "streaming is intentionally unsupported; keep AGENT_A2A_STREAMING=false")
			return
		}
		if parsed.Model == "" {
			parsed.Model = defaultModel
		}
		// The same probe vocabulary as the Responses path, so the smoke can prove the
		// prompt and data-loss guards act on this shape too, not only on the agent's.
		writeJSON(writer, http.StatusOK, chatCompletion(parsed.Model, probeText(parsed.Messages)))
	})
	return mux
}

func responseText(parsed request) string {
	if parsed.Text.Format.Type == "json_schema" {
		indexes, ok := namedEntityIndexes(parsed.Input)
		if !ok {
			return `{"items":[]}`
		}
		items := make([]map[string]any, len(indexes))
		for index, itemIndex := range indexes {
			items[index] = map[string]any{"index": itemIndex, "entities": []any{}}
		}
		encoded, err := json.Marshal(map[string]any{"items": items})
		if err != nil {
			return `{"items":[]}`
		}
		return string(encoded)
	}
	return probeText(parsed.Input)
}

// probeText maps the guardrail probes onto a reply, reading the raw client payload
// so both the Responses input and the chat-completions messages answer identically.
func probeText(input json.RawMessage) string {
	switch {
	case bytes.Contains(input, []byte(requestMaskProbe)):
		if bytes.Contains(input, []byte(requestMaskPII)) {
			return RawRequestPIIReply
		}
		return MaskedRequestPIIReply
	case bytes.Contains(input, []byte(ResponseMaskProbe)):
		return ResponsePIIReply
	case bytes.Contains(input, []byte(ResponseRejectProbe)):
		return ResponseRejectPIIReply
	default:
		return ReplyText
	}
}

func namedEntityIndexes(input json.RawMessage) ([]int, bool) {
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil, false
	}
	var visit func(any) ([]int, bool)
	visit = func(candidate any) ([]int, bool) {
		switch typed := candidate.(type) {
		case string:
			var payload struct {
				Items []struct {
					Index int `json:"index"`
				} `json:"items"`
			}
			if err := json.Unmarshal([]byte(typed), &payload); err == nil && payload.Items != nil {
				indexes := make([]int, len(payload.Items))
				for index, item := range payload.Items {
					indexes[index] = item.Index
				}
				return indexes, true
			}
		case []any:
			for _, item := range typed {
				if indexes, ok := visit(item); ok {
					return indexes, true
				}
			}
		case map[string]any:
			for _, item := range typed {
				if indexes, ok := visit(item); ok {
					return indexes, true
				}
			}
		}
		return nil, false
	}
	return visit(value)
}

func chatCompletion(model, text string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-agentops-fake",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": text},
		}},
		"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18},
	}
}

func response(model, text string) map[string]any {
	return responseWithOutput(model, []any{map[string]any{
		"id": "msg-agentops-fake", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{
			"type": "output_text", "text": text, "annotations": []any{},
		}},
	}})
}

func responseWithOutput(model string, output []any) map[string]any {
	return map[string]any{
		"id":                  "resp-agentops-fake",
		"object":              "response",
		"created_at":          0,
		"completed_at":        0,
		"status":              "completed",
		"model":               model,
		"error":               nil,
		"incomplete_details":  nil,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"output":              output,
		"usage":               map[string]int{"input_tokens": 10, "output_tokens": 8, "total_tokens": 18},
	}
}

// decodeSingleObject reads exactly one bounded JSON object into target, answering
// the caller and returning false when the body is unusable. Both model endpoints
// share it so their request contracts cannot drift apart.
func decodeSingleObject(writer http.ResponseWriter, incoming *http.Request, target any) bool {
	defer func() { _ = incoming.Body.Close() }()
	decoder := json.NewDecoder(http.MaxBytesReader(writer, incoming.Body, 1<<20))
	if err := decoder.Decode(target); err != nil {
		writeBadRequest(writer, fmt.Sprintf("invalid request body: %v", err))
		return false
	}
	if decoder.Decode(&struct{}{}) == nil {
		writeBadRequest(writer, "request body must contain one JSON object")
		return false
	}
	return true
}

// The fixture rejects only malformed input, so the status is part of the helper
// rather than a parameter every call site repeats.
func writeBadRequest(writer http.ResponseWriter, message string) {
	payload := errorEnvelope{}
	payload.Error.Message = message
	writeJSON(writer, http.StatusBadRequest, payload)
}

func writeJSON(writer http.ResponseWriter, status int, payload any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(writer, "could not encode response", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

func Server(address string, logger *slog.Logger, script *Script) *http.Server {
	return &http.Server{
		Addr:              address,
		Handler:           Handler(script),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}
