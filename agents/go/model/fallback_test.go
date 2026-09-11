package model

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestFallbackEngagesOnlyWhenThePrimaryFailsBeforeResponding is the whole rule
// in one table. The distinction it draws — dead before the turn started versus
// dropped mid-turn — is what keeps two different completions from being spliced
// into one answer.
func TestFallbackEngagesOnlyWhenThePrimaryFailsBeforeResponding(t *testing.T) {
	t.Parallel()

	// Field order follows go vet's fieldalignment check rather than reading order.
	for name, testCase := range map[string]struct {
		primary       *stubLLM
		fallback      *stubLLM
		wantError     string
		wantSpoken    []string
		wantSecondary int
	}{
		"a dead primary fails over": {
			primary:       &stubLLM{name: "primary", failBefore: true},
			fallback:      &stubLLM{name: "secondary", reply: "from the fallback"},
			wantSpoken:    []string{"from the fallback"},
			wantSecondary: 1,
		},
		"a healthy primary answers and the fallback is never touched": {
			primary:       &stubLLM{name: "primary", reply: "from the primary"},
			fallback:      &stubLLM{name: "secondary", failBefore: true},
			wantSpoken:    []string{"from the primary"},
			wantSecondary: 0,
		},
		"a mid-turn failure is surfaced, not masked": {
			primary:       &stubLLM{name: "primary", reply: "half an answer", failAfter: true},
			fallback:      &stubLLM{name: "secondary", reply: "from the fallback"},
			wantSpoken:    []string{"half an answer"},
			wantError:     "primary",
			wantSecondary: 0,
		},
		"both down surfaces the fallback's failure": {
			primary:       &stubLLM{name: "primary", failBefore: true},
			fallback:      &stubLLM{name: "secondary", failBefore: true},
			wantError:     "secondary",
			wantSecondary: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			chain := NewFallback(testCase.primary, testCase.fallback)
			spoken, failure := drive(t, chain)

			if !slices.Equal(spoken, testCase.wantSpoken) {
				t.Errorf("responses = %q, want %q", spoken, testCase.wantSpoken)
			}
			switch {
			case testCase.wantError == "" && failure != nil:
				t.Errorf("error = %v, want nil", failure)
			case testCase.wantError != "":
				if failure == nil {
					t.Fatalf("error = nil, want one naming %q", testCase.wantError)
				}
				if !strings.Contains(failure.Error(), testCase.wantError) {
					t.Errorf("error = %q, want it to name %q", failure, testCase.wantError)
				}
				// Wrapping must keep the cause reachable, or the policy plane
				// cannot classify a model failure it did not raise itself.
				if !errors.Is(failure, errModelDown) {
					t.Errorf("error = %v, want errors.Is(err, errModelDown)", failure)
				}
			}
			if testCase.primary.calls != 1 {
				t.Errorf("primary calls = %d, want 1", testCase.primary.calls)
			}
			if testCase.fallback.calls != testCase.wantSecondary {
				t.Errorf("fallback calls = %d, want %d", testCase.fallback.calls, testCase.wantSecondary)
			}
		})
	}
}

// TestFallbackReportsThePrimaryName pins what ADK routes on. Structured-output
// handling keys off the model name, so a chain must report the model that
// normally answers rather than inventing a composite one.
func TestFallbackReportsThePrimaryName(t *testing.T) {
	t.Parallel()

	chain := NewFallback(&stubLLM{name: "primary"}, &stubLLM{name: "secondary"})
	if chain.Name() != "primary" {
		t.Errorf("Name() = %q, want %q", chain.Name(), "primary")
	}
}

// TestFallbackStopsWhenTheConsumerStops is the iterator contract, on both
// branches. A caller that breaks out of the range — ADK does exactly this when
// a turn is canceled — must not have the chain keep pulling from a model behind
// its back, whichever model is currently answering.
func TestFallbackStopsWhenTheConsumerStops(t *testing.T) {
	t.Parallel()

	for name, testCase := range map[string]struct {
		primary  *stubLLM
		fallback *stubLLM
		want     string
	}{
		"while the primary answers": {
			// The primary would fail on its next step; stopping first must leave
			// the fallback untouched rather than trigger it.
			primary:  &stubLLM{name: "primary", reply: "first", failAfter: true},
			fallback: &stubLLM{name: "secondary", reply: "from the fallback"},
			want:     "first",
		},
		"while the fallback answers": {
			primary:  &stubLLM{name: "primary", failBefore: true},
			fallback: &stubLLM{name: "secondary", reply: "from the fallback", failAfter: true},
			want:     "from the fallback",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			chain := NewFallback(testCase.primary, testCase.fallback)

			spoken := 0
			for response, err := range chain.GenerateContent(t.Context(), request(), false) {
				if err != nil {
					t.Errorf("error = %v, want nil before the consumer stops", err)
					break
				}
				if got := responseText(response); got != testCase.want {
					t.Errorf("response text = %q, want %q", got, testCase.want)
				}
				spoken++
				break
			}

			if spoken != 1 {
				t.Errorf("consumed %d responses, want 1", spoken)
			}
			// Each model was entered at most once, and the failure each stub was
			// primed to raise after its first response never surfaced.
			if testCase.primary.calls != 1 {
				t.Errorf("primary calls = %d, want 1", testCase.primary.calls)
			}
			if testCase.fallback.calls > 1 {
				t.Errorf("fallback calls = %d, want at most 1", testCase.fallback.calls)
			}
		})
	}
}
