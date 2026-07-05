package tests

import (
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/internal/mcpclient"
)

func TestOpsMCPToolsetConstructs(t *testing.T) {
	// mcptoolset connects lazily, so building it does not launch the server here.
	toolset, err := mcpclient.OpsMCPToolset()
	if err != nil {
		t.Fatalf("OpsMCPToolset: %v", err)
	}
	if toolset == nil {
		t.Fatal("expected a non-nil toolset")
	}
}
