package principal_test

import (
	"strings"
	"testing"

	"github.com/MLOps-Courses/agentops-open-course/agents/go/principal"
)

func TestNetworkAuthenticationStatesAreExplicit(t *testing.T) {
	t.Parallel()

	if _, state := principal.Network(t.Context()); state != principal.NotNetwork {
		t.Fatalf("plain context state = %v, want %v", state, principal.NotNetwork)
	}

	network := principal.MarkNetwork(t.Context())
	if _, state := principal.Network(network); state != principal.NetworkUnauthenticated {
		t.Fatalf("marked context state = %v, want %v", state, principal.NetworkUnauthenticated)
	}

	authenticated, err := principal.NewAuthenticated("  alice@example.test  ")
	if err != nil {
		t.Fatalf("NewAuthenticated() error = %v, want nil", err)
	}
	bound := principal.BindNetwork(network, authenticated)
	got, state := principal.Network(bound)
	if state != principal.NetworkAuthenticated {
		t.Fatalf("bound context state = %v, want %v", state, principal.NetworkAuthenticated)
	}
	if got.Subject() != "alice@example.test" {
		t.Errorf("subject = %q, want trimmed subject", got.Subject())
	}
}

func TestAnEmptySubjectCannotBecomeAuthenticated(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		"",
		"   ",
		"alice\nadmin",
		"josé@example.test",
		strings.Repeat("a", 256),
	} {
		if _, err := principal.NewAuthenticated(raw); err == nil {
			t.Errorf("NewAuthenticated(%q) error = nil, want a refusal", raw)
		}
	}
}

func TestAuthenticatedSubjectAcceptsBoundedVisibleASCII(t *testing.T) {
	t.Parallel()

	want := strings.Repeat("a", 255)
	authenticated, err := principal.NewAuthenticated("  " + want + "  ")
	if err != nil {
		t.Fatalf("NewAuthenticated() error = %v, want nil", err)
	}
	if got := authenticated.Subject(); got != want {
		t.Errorf("Subject() = %q, want the trimmed 255-byte subject", got)
	}
}
