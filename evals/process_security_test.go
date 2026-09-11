package evals

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestWaitForReadinessRefusesRedirect(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var capturedRequests atomic.Int64
			capture := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				capturedRequests.Add(1)
				writer.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(capture.Close)

			source := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Location", capture.URL+"/ready")
				writer.WriteHeader(status)
			}))
			t.Cleanup(source.Close)

			err := waitForReadiness(t.Context(), make(chan error), source.URL, "/healthz", time.Second)
			if got := capturedRequests.Load(); got != 0 {
				t.Fatalf("redirect target received %d readiness request(s), want zero", got)
			}
			if err == nil {
				t.Fatal("waitForReadiness() error = nil, want redirected readiness to time out")
			}
		})
	}
}
