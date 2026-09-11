package conventions

import (
	"strings"
	"testing"
)

func TestCheckHeadingOpenersCountsOnlyWhatAReaderReads(t *testing.T) {
	tests := []struct {
		name string
		page string
		want bool
	}{
		{
			name: "short opener passes",
			page: "## What a gateway is\n\nA gateway is a reverse proxy in the request path.\n",
		},
		{
			name: "stacked definition, gloss, and enumeration fails",
			page: "## Why route the agent's connections through one reverse proxy\n\n" +
				"A gateway here is a reverse proxy: one process in the request path, in front of " +
				"shared backends, holding the traffic decisions each connection would otherwise " +
				"carry for itself — which model endpoint answers, which tools are callable, how " +
				"fast a caller may push, what gets logged, who is allowed in.\n",
			want: true,
		},
		{
			// Markup renders as fewer words than it is written with, so a sentence must not
			// fail on shortcode and link syntax the reader never sees.
			name: "shortcodes and link targets are not words",
			page: "## Where the threshold lives\n\n" +
				"The floor sits on the [4.2. Testing]({{< relref \"/4. Quality/4.2. Testing.md\" >}}) task line.\n",
		},
		{
			name: "a section opening on a list is not measured",
			page: "## Which signals gate a merge\n\n- Format, lint, compile, and race-enabled tests are merge gates that block a change and name the defect they found in it.\n",
		},
		{
			name: "a section opening on a table is not measured",
			page: "## Which port carries which protocol\n\n| Port | Protocol | Owner | Why this port and not another one, which is a question every reader asks |\n",
		},
		{
			name: "a section opening on a shortcode is not measured",
			page: "## What the composition binds\n\n{{< include path=\"agents/go/compose/composition.go\" region=\"root-agent\" lang=\"go\" >}}\n",
		},
		{
			// The limit applies to the first sentence, not the paragraph: a page may open
			// with a short claim and then develop it at length.
			name: "only the first sentence counts",
			page: "## What grounding means\n\nAn answer is grounded when you can point at its evidence. " +
				"That property is what separates a reply assembled from tool results out of your own systems " +
				"from one assembled out of recollection, and the two are indistinguishable by reading them.\n",
		},
		{
			name: "prose inside a fence is not measured",
			page: "## What the doctor prints\n\n```text\nthe doctor probes one tier of prerequisites and reports what it found, installing nothing at all and starting nothing at all either\n```\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := checkHeadingOpeners("content/test.md", test.page)
			if got := len(problems) > 0; got != test.want {
				t.Fatalf("flagged = %v, want %v (problems = %v)", got, test.want, problems)
			}
			if test.want && !strings.Contains(problemMessages(problems), "the rule is 25") {
				t.Fatalf("message should state the limit: %s", problemMessages(problems))
			}
		})
	}
}
