package conventions

import (
	"strings"
	"testing"
)

// pageRuleCase is one accepting or rejecting fixture for a per-page rule. An empty
// `want` means the rule must accept the text; otherwise the problems must name it.
type pageRuleCase struct {
	name  string
	where string
	text  string
	want  string
}

func runPageRuleCases(t *testing.T, check func(where, text string) []Problem, cases []pageRuleCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			where := test.where
			if where == "" {
				where = "content/2. Agents/2.1. First Agent.md"
			}
			problems := check(where, test.text)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %#v", problems)
				}
				return
			}
			if !strings.Contains(problemMessages(problems), test.want) {
				t.Fatalf("problems = %#v", problems)
			}
		})
	}
}

const glanceBlock = glanceOpen + "\n\n- **You will:** Outcome.\n" +
	"- **You need:** Precondition.\n- **Time:** about 20 minutes, hands-on. {{% /admonition %}}\n"

func TestCheckHeadings(t *testing.T) {
	runPageRuleCases(t, checkHeadings, []pageRuleCase{
		{name: "declarative", text: "## The claim this proves\n\n## And another\n"},
		{name: "one question is the page's tension", text: "## The claim\n\n## Is that enough?\n"},
		{name: "question with an anchor override", text: "## First?\n\n## Second? {#second}\n", want: "2 H2s ask a question"},
		{name: "no sections", text: "prose only\n", want: "expected at least one H2"},
		{
			name:  "glossary scans by question",
			where: "content/0. Overview/0.8. Glossary.md",
			text:  "## What is an agent?\n\n## What is a span?\n",
		},
	})
}

func TestCheckSceneHeadings(t *testing.T) {
	runPageRuleCases(t, checkSceneHeadings, []pageRuleCase{
		{name: "purpose heading", text: "## Why the agent has to become a container image\n"},
		{name: "port is not a clock", text: "## Free port 11434 before the cluster can reach Ollama\n"},
		{name: "latency figure", text: "## Span p95 exceeds 15s under load\n"},
		{
			name: "scene heading",
			text: "## Ana gets paged at 02:14\n",
			want: "names a moment in the worked example",
		},
		{
			name: "clock inside a fence is prose the page quotes",
			text: "## A claim\n\n```text\nHH:MM 03:00 alert fired\n```\n",
		},
	})
}

func TestCheckRetiredNarrative(t *testing.T) {
	runPageRuleCases(t, checkRetiredNarrative, []pageRuleCase{
		{name: "role instead of character", text: "The approving engineer supplies a rationale.\n"},
		{name: "seed data still allowed", text: "`INC-002` is a SEV1 in the committed seed.\n"},
		{name: "substring is not the persona", text: "Run the analysis over every package.\n"},
		{
			name: "persona",
			text: "Ana approves a restart of the inventory service.\n",
			want: "retired narrative persona or brand",
		},
		{
			name: "brand",
			text: "Northwind Retail does not exist.\n",
			want: "retired narrative persona or brand",
		},
		{
			name: "capture may quote whatever it printed",
			text: "## A claim\n\n```text\napprover=Ana\n```\n",
		},
	})
}

func TestCheckClosing(t *testing.T) {
	runPageRuleCases(t, checkClosing, []pageRuleCase{
		{name: "course page", text: "## A claim\n\n## What you can do now\n"},
		{name: "chapter index", text: "## A claim\n\n## What this chapter proved\n"},
		{name: "lookup page", text: "## A claim\n\n## How to use this page later\n"},
		{name: "retired question spelling", text: "## A claim\n\n## What proves this page worked?\n", want: "last H2 must be one of"},
		{name: "no closing heading", text: "## A claim\n\n## Something else\n", want: "last H2 must be one of"},
	})
}

func TestCheckKindAndDeclaredKind(t *testing.T) {
	runPageRuleCases(t, checkKind, []pageRuleCase{
		{name: "one kind", text: "- **Time:** about 20 minutes, hands-on.\n"},
		{name: "no kind", text: "- **Time:** about 20 minutes.\n", want: "must name exactly one page kind"},
		{name: "two kinds", text: "- **Time:** about 20 minutes, hands-on, then reference.\n", want: "must name exactly one page kind"},
		{name: "no time line", text: "prose only\n", want: "must name exactly one page kind"},
	})
	if got := declaredKind("- **Time:** about 8 minutes, orientation. {{% /admonition %}}\n"); got != "orientation" {
		t.Errorf("declaredKind = %q, want orientation", got)
	}
}

func TestCheckGlance(t *testing.T) {
	runPageRuleCases(t, checkGlance, []pageRuleCase{
		{name: "complete", text: glanceBlock + "\n## A claim\n"},
		{name: "missing a field", text: glanceOpen + "\n\n- **You will:** Outcome.\n\n## A claim\n", want: "**You need:**"},
		{name: "no glance block", text: "## A claim\n", want: "In one glance"},
		{
			name: "glance below the first H2 does not count",
			text: "## A claim\n\n" + glanceBlock,
			want: "In one glance",
		},
	})
}

func TestCheckCollapsibles(t *testing.T) {
	deeper := `{{% collapsible note "Deeper: the reason" %}}` + "\n"
	runPageRuleCases(t, checkCollapsibles, []pageRuleCase{
		{name: "none", text: "## A claim\n"},
		{name: "three", text: strings.Repeat(deeper, 3)},
		{name: "four", text: strings.Repeat(deeper, 4), want: "4 collapsibles"},
		{name: "wrong summary prefix", text: `{{% collapsible note "Aside: the reason" %}}` + "\n", want: `must start with "Deeper: "`},
	})
}

func TestCheckLinkLabels(t *testing.T) {
	runPageRuleCases(t, checkLinkLabels, []pageRuleCase{
		{name: "full page name", text: `See [2.1. First Agent]({{< relref "/2. Agents/2.1. First Agent.md" >}}).` + "\n"},
		{name: "bare number", text: `See [2.1]({{< relref "/2. Agents/2.1. First Agent.md" >}}).` + "\n", want: "not a bare number: [2.1]"},
	})
}

func TestCheckGoBlocks(t *testing.T) {
	runPageRuleCases(t, checkGoBlocks, []pageRuleCase{
		{name: "declared simplified", text: "```go\n// simplified\nfunc main() {}\n```\n"},
		{name: "unowned in chapter 2", text: "```go\nfunc main() {}\n```\n", want: "unowned go blocks"},
		{
			name:  "chapter 6 is outside the rule",
			where: "content/6. Platform/6.0. Platform.md",
			text:  "```go\nfunc main() {}\n```\n",
		},
	})
}

func TestCheckAdmonitions(t *testing.T) {
	runPageRuleCases(t, checkAdmonitions, []pageRuleCase{
		{name: "supported", text: `{{% admonition abstract "In one glance" %}}` + "\n"},
		{name: "unsupported", text: `{{% admonition caution "Careful" %}}` + "\n", want: `unsupported admonition type "caution"`},
		{name: "inside a fence is quoted, not used", text: "```markdown\n" + `{{% admonition caution "Careful" %}}` + "\n```\n"},
	})
}

func TestCheckOrderedLists(t *testing.T) {
	runPageRuleCases(t, checkOrderedLists, []pageRuleCase{
		{name: "repeated one", text: "1. first\n1. second\n"},
		{name: "hand numbered", text: "1. first\n2. second\n", want: "must use `1.`, not `2.`"},
		{name: "inside a fence", text: "```text\n1. first\n2. second\n```\n"},
	})
}

func TestCheckPageLinkTargets(t *testing.T) {
	runPageRuleCases(t, checkPageLinkTargets, []pageRuleCase{
		{name: "label matches target", text: `[2.1. First Agent]({{< relref "/2. Agents/2.1. First Agent.md"` + "\n"},
		{name: "label with a suffix", text: `[2.1. First Agent — the grounded turn]({{< relref "/2. Agents/2.1. First Agent.md"` + "\n"},
		{name: "label names another page", text: `[2.2. Models]({{< relref "/2. Agents/2.1. First Agent.md"` + "\n", want: "use the target's full page name"},
	})
}

func TestCheckHandsOnAction(t *testing.T) {
	command := "```bash\nmise run test\n```\n"
	runPageRuleCases(t, checkHandsOnAction, []pageRuleCase{
		{name: "command in the first section", text: glanceBlock + "\n## A claim\n\n" + command},
		{
			name: "command only in the third section",
			text: glanceBlock + "\n## One\n\n## Two\n\n## Three\n\n" + command,
			want: "must reach a bash/shell/console command",
		},
		{
			name: "orientation pages are outside the rule",
			text: "- **Time:** about 8 minutes, orientation.\n\n## One\n\n## Two\n\n## Three\n\nprose\n",
		},
	})
}

func TestCheckMachinePaths(t *testing.T) {
	runPageRuleCases(t, checkMachinePaths, []pageRuleCase{
		{name: "repository-relative", text: "Read `agents/go/compose/composition.go`.\n"},
		{name: "home directory", text: "Read `/home/ana/checkout/agents/go`.\n", want: "machine-specific path"},
		{name: "file uri", text: "It printed `file:///tmp/schema.json`.\n", want: "machine-specific path"},
		{name: "retired registry hostname", text: "Push to `k3d-registry.localhost:5050`.\n", want: "obsolete registry hostname"},
	})
}

func TestCheckClosingCadence(t *testing.T) {
	cadence := "## What you can do now\n\n- A capability.\n\nAn hour ago this was a mystery. It is now a file.\n"
	plain := "## What you can do now\n\n- A capability.\n\nContinue to the next page.\n"
	// A time marker above the last H2 is ordinary prose, not a closing cadence.
	midPage := "## A claim\n\nA tutorial written six months ago can describe a schema that no longer exists.\n\n" + plain
	tests := []struct {
		name  string
		pages pageSet
		want  string
	}{
		{
			name:  "one keeper per chapter",
			pages: pageSet{"content/1. Setup/1.0. System.md": cadence, "content/1. Setup/1.1. Go.md": plain},
		},
		{
			name:  "mid-page time marker is not a closer",
			pages: pageSet{"content/0. Overview/0.6. Resources.md": midPage, "content/0. Overview/0.2. Evidence.md": cadence},
		},
		{
			name:  "a second page in the same chapter",
			pages: pageSet{"content/1. Setup/1.0. System.md": cadence, "content/1. Setup/1.1. Go.md": cadence},
			want:  "closes on the time-marker cadence",
		},
		{
			name:  "a chapter with no keeper at all",
			pages: pageSet{"content/4. Quality/4.1. Linting.md": cadence},
			want:  "no page in it",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := checkClosingCadence(test.pages)
			if test.want == "" {
				if len(problems) != 0 {
					t.Fatalf("problems = %#v", problems)
				}
				return
			}
			if !strings.Contains(problemMessages(problems), test.want) {
				t.Fatalf("problems = %#v", problems)
			}
		})
	}
}

func TestCheckImages(t *testing.T) {
	runPageRuleCases(t, checkImages, []pageRuleCase{
		{name: "described capture", text: "![ADK Events view for one turn, showing a functionCall to list_incidents.](/images/events.png)\n"},
		{name: "empty alt", text: "![](/images/events.png)\n", want: "needs alt text"},
		{name: "whitespace alt", text: "![   ](/images/events.png)\n", want: "needs alt text"},
		{name: "assets path 404s", text: "![Something real.](/assets/images/events.png)\n", want: "silently 404s"},
		{name: "a link is not an image", text: "[0.2. Evidence](/0-overview/0-2-evidence/)\n"},
		{name: "quoted inside a fence", text: "```markdown\n![](/assets/images/x.png)\n```\n"},
	})
}
