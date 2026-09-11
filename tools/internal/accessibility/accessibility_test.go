package accessibility

import (
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDurationSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  float64
	}{
		{name: "seconds", value: "2s", want: 2},
		{name: "milliseconds", value: "150ms", want: 0.15},
		{name: "longest list item", value: "10ms, 0.25s, 100ms", want: 0.25},
		{name: "empty", value: "", want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := durationSeconds(test.value)
			if err != nil {
				t.Fatalf("durationSeconds(%q): %v", test.value, err)
			}
			if math.Abs(got-test.want) > 0.000_001 {
				t.Fatalf("durationSeconds(%q) = %v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestDurationSecondsRejectsInvalidCSS(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"fast", "10", "-2s", "1minute"} {
		if _, err := durationSeconds(value); err == nil {
			t.Fatalf("durationSeconds(%q) succeeded, want an error", value)
		}
	}
}

func TestContrastRatio(t *testing.T) {
	t.Parallel()

	if got := contrastRatio(rgb{0, 0, 0}, rgb{255, 255, 255}); math.Abs(got-21) > 0.001 {
		t.Fatalf("black on white ratio = %v, want 21", got)
	}
	if got := contrastRatio(rgb{255, 255, 255}, rgb{0, 0, 0}); math.Abs(got-21) > 0.001 {
		t.Fatalf("white on black ratio = %v, want 21", got)
	}
}

func TestIsLocalRequestURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		url  string
		want bool
	}{
		{url: "http://127.0.0.1:4321/index.html", want: true},
		{url: "http://localhost:4321/", want: true},
		{url: "http://[::1]:4321/", want: true},
		{url: "data:image/svg+xml;base64,PHN2Zy8+", want: true},
		{url: "blob:http://127.0.0.1:4321/id", want: true},
		{url: "about:blank", want: true},
		{url: "https://example.com/font.woff2", want: false},
		{url: "http://localhost.example.com/", want: false},
		{url: "file:///etc/passwd", want: false},
		{url: "not a URL", want: false},
	}
	for _, test := range tests {
		if got := isLocalRequestURL(test.url); got != test.want {
			t.Errorf("isLocalRequestURL(%q) = %t, want %t", test.url, got, test.want)
		}
	}
}

func TestValidateAXTree(t *testing.T) {
	t.Parallel()

	nodes := []axNode{
		{Role: "main"},
		{Role: "heading", Name: "Course home"},
		{Role: "button", Name: "Search"},
		{Role: "textbox", Name: "Message"},
	}
	if err := validateAXTree(nodes, "Course home", "homepage"); err != nil {
		t.Fatalf("validateAXTree(valid): %v", err)
	}
}

func TestValidateAXTreeRejectsMissingSemantics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		want  string
		name  string
		nodes []axNode
	}{
		{
			name:  "missing main",
			nodes: []axNode{{Role: "heading", Name: "Course home"}},
			want:  "exactly one main landmark",
		},
		{
			name:  "missing heading",
			nodes: []axNode{{Role: "main"}},
			want:  "does not expose the H1",
		},
		{
			name: "unnamed control",
			nodes: []axNode{
				{Role: "main"},
				{Role: "heading", Name: "Course home"},
				{Role: "button"},
			},
			want: "unnamed interactive controls",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateAXTree(test.nodes, "Course home", "homepage")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateAXTree() error = %v, want text %q", err, test.want)
			}
		})
	}
}

func TestResolveChromePathUsesConfiguredExecutable(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "chrome")
	mode := os.FileMode(0o755)
	if runtime.GOOS == "windows" {
		path += ".exe"
		mode = 0o644
	}
	if err := os.WriteFile(path, []byte("browser fixture"), mode); err != nil {
		t.Fatalf("write Chrome fixture: %v", err)
	}

	got, err := resolveChromePath(path)
	if err != nil {
		t.Fatalf("resolveChromePath(%q): %v", path, err)
	}
	if got != path {
		t.Fatalf("resolveChromePath(%q) = %q, want configured path", path, got)
	}
}

func TestResolveChromePathRejectsBadConfiguredPath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "missing-chrome")
	_, err := resolveChromePath(path)
	if err == nil || !strings.Contains(err.Error(), "CHROME_PATH") {
		t.Fatalf("resolveChromePath(%q) error = %v, want clear CHROME_PATH failure", path, err)
	}
}
