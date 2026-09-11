package accessibility

import (
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

type rgb [3]float64

type axNode struct {
	Role string
	Name string
}

func durationSeconds(value string) (float64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}

	longest := 0.0
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		var divisor float64
		switch {
		case strings.HasSuffix(item, "ms"):
			item = strings.TrimSuffix(item, "ms")
			divisor = 1000
		case strings.HasSuffix(item, "s"):
			item = strings.TrimSuffix(item, "s")
			divisor = 1
		default:
			return 0, fmt.Errorf("invalid CSS duration %q", item)
		}
		seconds, err := strconv.ParseFloat(item, 64)
		if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
			return 0, fmt.Errorf("invalid CSS duration %q", item)
		}
		seconds /= divisor
		longest = max(longest, seconds)
	}
	return longest, nil
}

func luminance(color rgb) float64 {
	linear := func(value float64) float64 {
		value /= 255
		if value <= 0.04045 {
			return value / 12.92
		}
		return math.Pow((value+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(color[0]) + 0.7152*linear(color[1]) + 0.0722*linear(color[2])
}

func contrastRatio(foreground, background rgb) float64 {
	values := []float64{luminance(foreground), luminance(background)}
	slices.Sort(values)
	return (values[1] + 0.05) / (values[0] + 0.05)
}

func isLocalRequestURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "about":
		return parsed.Opaque == "blank"
	case "data":
		return true
	case "blob":
		return isLocalRequestURL(strings.TrimPrefix(raw, "blob:"))
	case "http", "https":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	default:
		return false
	}
}

func validateAXTree(nodes []axNode, heading, label string) error {
	mainCount := 0
	headingFound := false
	unnamed := make([]string, 0)
	interactiveRoles := map[string]struct{}{
		"button":    {},
		"checkbox":  {},
		"combobox":  {},
		"radio":     {},
		"searchbox": {},
		"textbox":   {},
	}
	for _, node := range nodes {
		if node.Role == "main" {
			mainCount++
		}
		if node.Role == "heading" && strings.TrimSpace(node.Name) == heading {
			headingFound = true
		}
		if _, interactive := interactiveRoles[node.Role]; interactive && strings.TrimSpace(node.Name) == "" {
			unnamed = append(unnamed, node.Role)
		}
	}
	if mainCount != 1 {
		return fmt.Errorf("%s: accessibility tree needs exactly one main landmark", label)
	}
	if !headingFound {
		return fmt.Errorf("%s: accessibility tree does not expose the H1 %q", label, heading)
	}
	if len(unnamed) > 0 {
		return fmt.Errorf("%s: accessibility tree has unnamed interactive controls: %v", label, unnamed)
	}
	return nil
}

func resolveChromePath(configured string) (string, error) {
	if configured != "" {
		path, err := exec.LookPath(configured)
		if err != nil {
			return "", fmt.Errorf("CHROME_PATH %q is not an executable Chrome binary: %w", configured, err)
		}
		return path, nil
	}

	candidates := []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	switch runtime.GOOS {
	case "darwin":
		candidates = append([]string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		}, candidates...)
	case "windows":
		candidates = append([]string{"chrome.exe"}, candidates...)
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("system Chrome is unavailable; install Chrome or set CHROME_PATH to its executable")
}
