package staticserver

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerServesTheSelectedDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("agentops client"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := Server("127.0.0.1:0", directory, slog.New(slog.DiscardHandler))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	server.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "agentops client") {
		t.Fatalf("response = %d %q", response.Code, response.Body.String())
	}
}
