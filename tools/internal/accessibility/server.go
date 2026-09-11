package accessibility

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
)

const maxRPCBodyBytes = 64 << 10

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcTask struct {
	Kind      string        `json:"kind"`
	ID        string        `json:"id"`
	ContextID string        `json:"contextId"`
	Status    rpcTaskStatus `json:"status"`
	Artifacts []any         `json:"artifacts"`
}

type rpcTaskStatus struct {
	State string `json:"state"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  rpcTask         `json:"result"`
}

type fakeRPC struct {
	err    error
	counts map[string]int
	mu     sync.Mutex
}

func newFakeRPC() *fakeRPC {
	return &fakeRPC{counts: make(map[string]int)}
}

func (fake *fakeRPC) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/" {
		http.Error(writer, "not found", http.StatusNotFound)
		return
	}

	request.Body = http.MaxBytesReader(writer, request.Body, maxRPCBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var rpc rpcRequest
	if err := decoder.Decode(&rpc); err != nil {
		fake.recordError(fmt.Errorf("decode web-client RPC: %w", err))
		http.Error(writer, "invalid RPC", http.StatusBadRequest)
		return
	}
	if err := requireJSONEOF(decoder); err != nil {
		fake.recordError(err)
		http.Error(writer, "invalid RPC", http.StatusBadRequest)
		return
	}

	state, taskID, err := fake.validate(rpc)
	if err != nil {
		fake.recordError(err)
	}
	if len(rpc.ID) == 0 {
		rpc.ID = json.RawMessage("null")
	}
	writer.Header().Set("Content-Type", "application/json")
	if encodeErr := json.NewEncoder(writer).Encode(rpcResponse{
		JSONRPC: "2.0",
		ID:      rpc.ID,
		Result: rpcTask{
			Kind:      "task",
			ID:        taskID,
			ContextID: "browser-acceptance",
			Status:    rpcTaskStatus{State: state},
			Artifacts: []any{},
		},
	}); encodeErr != nil {
		fake.recordError(fmt.Errorf("encode web-client RPC: %w", encodeErr))
	}
}

func (fake *fakeRPC) validate(request rpcRequest) (state, taskID string, err error) {
	fake.mu.Lock()
	fake.counts[request.Method]++
	fake.mu.Unlock()

	if request.JSONRPC != "2.0" {
		return "failed", "invalid-jsonrpc", fmt.Errorf("web client sent JSON-RPC version %q", request.JSONRPC)
	}
	switch request.Method {
	case "tasks/cancel":
		var params map[string]any
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return "failed", "task-cancel", fmt.Errorf("decode cancellation params: %w", err)
		}
		want := map[string]any{"id": "task-cancel"}
		if !reflect.DeepEqual(params, want) {
			return "failed", "task-cancel", fmt.Errorf("web client sent the wrong cancellation target: %v", params)
		}
		return "canceled", "task-cancel", nil
	case "message/send":
		if err := validateDenial(request.Params); err != nil {
			return "failed", "task-approval", err
		}
		return "completed", "task-approval", nil
	default:
		return "failed", "unexpected-method", fmt.Errorf("web client sent unexpected RPC method %q", request.Method)
	}
}

func validateDenial(raw json.RawMessage) error {
	var params struct {
		Message struct {
			Parts []struct {
				Data struct {
					Response struct {
						Confirmed *bool `json:"confirmed"`
					} `json:"response"`
				} `json:"data"`
				Metadata struct {
					ADKType string `json:"adk_type"`
				} `json:"metadata"`
			} `json:"parts"`
		} `json:"message"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return fmt.Errorf("decode denial response: %w", err)
	}
	if len(params.Message.Parts) == 0 {
		return errors.New("web client denial response has no message parts")
	}
	part := params.Message.Parts[0]
	if part.Data.Response.Confirmed == nil || *part.Data.Response.Confirmed || part.Metadata.ADKType != "function_response" {
		return errors.New("web client sent the wrong denial response")
	}
	return nil
}

func (fake *fakeRPC) recordError(err error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.err == nil {
		fake.err = err
	}
}

func (fake *fakeRPC) verify() error {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.err != nil {
		return fake.err
	}
	for _, method := range []string{"tasks/cancel", "message/send"} {
		if fake.counts[method] != 1 {
			return fmt.Errorf("web client sent %s %d times, want exactly once", method, fake.counts[method])
		}
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("web-client RPC contains trailing JSON")
		}
		return fmt.Errorf("decode trailing web-client RPC: %w", err)
	}
	return nil
}

func startStaticServer(directory string, rpc *fakeRPC) *httptest.Server {
	files := http.FileServer(http.Dir(directory))
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if rpc != nil && request.Method == http.MethodPost && request.URL.Path == "/" {
			rpc.ServeHTTP(writer, request)
			return
		}
		files.ServeHTTP(writer, request)
	})
	return httptest.NewServer(handler)
}
