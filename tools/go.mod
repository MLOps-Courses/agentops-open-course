module github.com/MLOps-Courses/agentops-open-course/tools

go 1.26.6

tool (
	golang.org/x/tools/cmd/goimports
	golang.org/x/vuln/cmd/govulncheck
	mvdan.cc/gofumpt
)

require (
	github.com/chromedp/cdproto v0.0.0-20260714215040-dc233986426f // compatibility hold: owner=chromedp@v0.16.0 constraint=dc233986426f validator=real Chrome accessibility acceptance
	github.com/chromedp/chromedp v0.16.0
	github.com/pelletier/go-toml v1.9.5
	golang.org/x/net v0.58.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/tools v0.48.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	mvdan.cc/gofumpt v0.11.0 // indirect
)
