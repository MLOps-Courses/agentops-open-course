package evals

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestMalformedBaseURLOmitsCredentials(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-secret-do-not-print"
	malformed := "http://agent:" + secret + "@%zz"
	for name, construct := range map[string]func() error{
		"REST": func() error {
			_, err := NewRESTClient(RESTClientConfig{BaseURL: malformed})
			return err
		},
		"A2A": func() error {
			_, err := NewA2AClient(A2AClientConfig{BaseURL: malformed})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := construct()
			if err == nil {
				t.Fatal("client constructor error = nil, want malformed URL to fail")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("client constructor error leaked URL userinfo: %v", err)
			}
		})
	}
}

func TestClientConstructorsRejectUndialableBaseURLPorts(t *testing.T) {
	t.Parallel()

	for _, baseURL := range []string{
		"http://127.0.0.1:",
		"http://127.0.0.1:0",
		"http://127.0.0.1:65536",
	} {
		for name, construct := range map[string]func() error{
			"REST": func() error {
				_, err := NewRESTClient(RESTClientConfig{BaseURL: baseURL})
				return err
			},
			"A2A": func() error {
				_, err := NewA2AClient(A2AClientConfig{BaseURL: baseURL})
				return err
			},
		} {
			t.Run(name+"/"+baseURL, func(t *testing.T) {
				if err := construct(); err == nil {
					t.Fatal("client constructor error = nil, want invalid port rejection before dialing")
				}
			})
		}
	}
}

const syntheticProviderDetail = "password=SYNTHETIC_DO_NOT_USE_PROVIDER_BODY_123456"

type failingProviderBody struct{}

func (failingProviderBody) Read([]byte) (int, error) {
	return 0, errors.New("SYNTHETIC_PROVIDER_READ_DETAIL_DO_NOT_LOG")
}

func (failingProviderBody) Close() error { return nil }

type failingResponseRoundTripper struct{}

func (failingResponseRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       failingProviderBody{},
		Request:    request,
	}, nil
}

type failingRoundTripper struct{ err error }

func (transport failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, transport.err
}

func TestHTTPClientFailuresOmitTransportDetails(t *testing.T) {
	t.Parallel()

	const marker = "SYNTHETIC_TRANSPORT_DETAIL_DO_NOT_LOG"
	client := &http.Client{Transport: failingRoundTripper{err: errors.New(marker)}}
	for name, test := range map[string]struct {
		call func() error
		want string
	}{
		"REST": {
			call: func() error {
				rest, err := NewRESTClient(RESTClientConfig{Client: client, BaseURL: "http://example.test"})
				if err != nil {
					return err
				}
				_, err = rest.Send(t.Context(), "session", "status")
				return err
			},
			want: "run REST turn: send REST request",
		},
		"REST SSE": {
			call: func() error {
				rest, err := NewRESTClient(RESTClientConfig{
					Client: client, BaseURL: "http://example.test", Streaming: true,
				})
				if err != nil {
					return err
				}
				_, err = rest.Send(t.Context(), "session", "status")
				return err
			},
			want: "send REST SSE request",
		},
		"A2A": {
			call: func() error {
				a2a, err := NewA2AClient(A2AClientConfig{Client: client, BaseURL: "http://example.test"})
				if err != nil {
					return err
				}
				_, err = a2a.Send(t.Context(), "session", "status")
				return err
			},
			want: "send A2A request",
		},
		"judge": {
			call: func() error {
				judge, err := NewGatewayJudge(GatewayJudgeConfig{
					Client: client, BaseURL: "http://example.test", Model: "judge", APIKey: "marker",
				})
				if err != nil {
					return err
				}
				_, err = judge.Judge(t.Context(), JudgeInput{
					Questions: []string{"question"}, Answers: []string{"answer"}, ReferenceAnswers: []string{"reference"},
				})
				return err
			},
			want: "call gateway judge",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := test.call()
			if err == nil {
				t.Fatal("request error = nil, want injected transport failure")
			}
			if got := err.Error(); got != test.want {
				t.Fatalf("request error = %q, want fixed local class %q", got, test.want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("request error retained an injected transport detail")
			}
		})
	}
}

func TestSuccessResponseReadFailuresOmitProviderDetails(t *testing.T) {
	t.Parallel()

	const marker = "SYNTHETIC_PROVIDER_READ_DETAIL_DO_NOT_LOG"
	client := &http.Client{Transport: failingResponseRoundTripper{}}
	for name, test := range map[string]struct {
		call func() error
		want string
	}{
		"REST": {
			call: func() error {
				rest, err := NewRESTClient(RESTClientConfig{Client: client, BaseURL: "http://example.test"})
				if err != nil {
					return err
				}
				_, err = rest.Send(t.Context(), "session", "status")
				return err
			},
			want: "run REST turn: decode REST response",
		},
		"REST SSE": {
			call: func() error {
				rest, err := NewRESTClient(RESTClientConfig{
					Client: client, BaseURL: "http://example.test", Streaming: true,
				})
				if err != nil {
					return err
				}
				_, err = rest.Send(t.Context(), "session", "status")
				return err
			},
			want: "read REST SSE response",
		},
		"A2A": {
			call: func() error {
				a2a, err := NewA2AClient(A2AClientConfig{Client: client, BaseURL: "http://example.test"})
				if err != nil {
					return err
				}
				_, err = a2a.Send(t.Context(), "session", "status")
				return err
			},
			want: "read A2A stream response",
		},
		"judge": {
			call: func() error {
				judge, err := NewGatewayJudge(GatewayJudgeConfig{
					Client: client, BaseURL: "http://example.test", Model: "judge", APIKey: "marker",
				})
				if err != nil {
					return err
				}
				_, err = judge.Judge(t.Context(), JudgeInput{
					Questions: []string{"question"}, Answers: []string{"answer"}, ReferenceAnswers: []string{"reference"},
				})
				return err
			},
			want: "decode judge response",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := test.call()
			if err == nil {
				t.Fatal("request error = nil, want injected response read failure")
			}
			if got := err.Error(); got != test.want {
				t.Fatalf("request error = %q, want fixed local class %q", got, test.want)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatal("request error retained a provider-controlled read failure")
			}
		})
	}
}

func TestProviderStatusErrorsNeverPersistResponseBodies(t *testing.T) {
	t.Parallel()

	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Body: io.NopCloser(strings.NewReader(
			`{"error":"synthetic body with password=not-a-real-secret-value"}`,
		)),
	}
	err := responseStatusError(response)
	if err == nil {
		t.Fatal("responseStatusError() = nil, want an HTTP status error")
	}
	if !strings.Contains(err.Error(), "HTTP 429") {
		t.Errorf("error = %q, want the status code", err)
	}
	for _, forbidden := range []string{"synthetic body", "not-a-real-secret-value"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error = %q, want provider body %q omitted", err, forbidden)
		}
	}
}

func TestProviderStatusErrorsNeverPersistBodyReadFailures(t *testing.T) {
	t.Parallel()

	response := &http.Response{StatusCode: http.StatusBadGateway, Body: failingProviderBody{}}
	err := responseStatusError(response)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("responseStatusError() = %v, want the HTTP status", err)
	}
	for _, forbidden := range []string{"SYNTHETIC_PROVIDER_READ_DETAIL_DO_NOT_LOG"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error = %q, want body read failure %q omitted", err, forbidden)
		}
	}
}

func TestRESTStreamingErrorEventsOmitProviderData(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: error\ndata: "+syntheticProviderDetail+"\n\n")
	}))
	defer server.Close()
	client, err := NewRESTClient(RESTClientConfig{BaseURL: server.URL, Streaming: true})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Send(context.Background(), "session", "status")
	if err == nil || !strings.Contains(err.Error(), "REST SSE error event") {
		t.Fatalf("Send() error = %v, want the local SSE failure class", err)
	}
	if strings.Contains(err.Error(), syntheticProviderDetail) {
		t.Fatalf("Send() retained provider SSE data: %q", err)
	}
}

func TestA2AJSONRPCErrorsOmitProviderMessages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(writer).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      input.ID,
			"error": map[string]any{
				"code": -32000, "message": syntheticProviderDetail,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()
	client, err := NewA2AClient(A2AClientConfig{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Send(context.Background(), "session", "status")
	if err == nil || !strings.Contains(err.Error(), "JSON-RPC error -32000") {
		t.Fatalf("Send() error = %v, want the local JSON-RPC code", err)
	}
	if strings.Contains(err.Error(), syntheticProviderDetail) {
		t.Fatalf("Send() retained a provider JSON-RPC message: %q", err)
	}
}

const (
	redirectRequestSecret  = "synthetic-cross-origin-request-secret"
	redirectLocationMarker = "synthetic-cross-origin-location"
)

func TestBodyBearingClientsRefuseCrossOriginRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for name, send := range map[string]func(*http.Client, string) error{
			"REST": func(httpClient *http.Client, baseURL string) error {
				client, err := NewRESTClient(RESTClientConfig{Client: httpClient, BaseURL: baseURL})
				if err != nil {
					return err
				}
				_, err = client.Send(t.Context(), "session", redirectRequestSecret)
				return err
			},
			"REST SSE": func(httpClient *http.Client, baseURL string) error {
				client, err := NewRESTClient(RESTClientConfig{Client: httpClient, BaseURL: baseURL, Streaming: true})
				if err != nil {
					return err
				}
				_, err = client.Send(t.Context(), "session", redirectRequestSecret)
				return err
			},
			"A2A": func(httpClient *http.Client, baseURL string) error {
				client, err := NewA2AClient(A2AClientConfig{Client: httpClient, BaseURL: baseURL})
				if err != nil {
					return err
				}
				_, err = client.Send(t.Context(), "session", redirectRequestSecret)
				return err
			},
			"judge": func(httpClient *http.Client, baseURL string) error {
				judge, err := NewGatewayJudge(GatewayJudgeConfig{
					Client: httpClient, BaseURL: baseURL, Model: "judge", APIKey: "local-marker",
				})
				if err != nil {
					return err
				}
				_, err = judge.Judge(t.Context(), JudgeInput{
					Questions: []string{redirectRequestSecret}, Answers: []string{"answer"}, ReferenceAnswers: []string{"reference"},
				})
				return err
			},
		} {
			t.Run(fmt.Sprintf("%s/%d", name, status), func(t *testing.T) {
				var capturedRequests atomic.Int64
				var capturedSecret atomic.Bool
				capture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					capturedRequests.Add(1)
					body, _ := io.ReadAll(io.LimitReader(request.Body, 1<<20))
					capturedSecret.Store(strings.Contains(string(body), redirectRequestSecret))
					http.Error(writer, "redirect target refuses the request", http.StatusBadGateway)
				}))
				t.Cleanup(capture.Close)

				location := capture.URL + "/" + redirectLocationMarker
				source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Location", location)
					writer.WriteHeader(status)
				}))
				t.Cleanup(source.Close)

				var callerRedirects atomic.Int64
				callerClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
					callerRedirects.Add(1)
					return nil
				}}
				err := send(callerClient, source.URL)
				if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("HTTP %d", status)) {
					t.Fatalf("request error = %v, want the local redirect status", err)
				}
				if got := capturedRequests.Load(); got != 0 || capturedSecret.Load() {
					t.Fatalf("redirect target received %d request(s), want zero cross-origin body replays", got)
				}
				for _, forbidden := range []string{redirectRequestSecret, redirectLocationMarker, capture.URL} {
					if strings.Contains(err.Error(), forbidden) {
						t.Fatalf("request error retained redirect-controlled detail")
					}
				}

				// Constructors must harden a clone, not mutate the caller-owned client.
				if err := callerClient.CheckRedirect(nil, nil); err != nil || callerRedirects.Load() != 1 {
					t.Fatalf("caller-owned redirect policy was mutated")
				}
			})
		}
	}
}
