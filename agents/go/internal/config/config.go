// Package config holds the agent's typed, fail-fast configuration, sourced from the environment.
package config

import (
	"os"
	"path/filepath"
	"runtime"
)

// DefaultModel is set explicitly because ADK model defaults churn.
// Native Gemini in Ch. 2-4; other providers arrive via agentgateway in Ch. 5.
const DefaultModel = "gemini-3.5-flash"

// Config is the agent configuration.
type Config struct {
	Model   string // LLM identifier.
	APIKey  string // GOOGLE_API_KEY for the native Gemini API.
	DataDir string // Directory holding the bundled Ops Copilot dataset (incidents.db, runbooks/).
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	model := os.Getenv("AGENT_MODEL")
	if model == "" {
		model = DefaultModel
	}
	return Config{
		Model:   model,
		APIKey:  os.Getenv("GOOGLE_API_KEY"),
		DataDir: dataDir(),
	}
}

// dataDir resolves the bundled dataset directory. AGENT_DATA_DIR wins (e.g. in containers,
// Ch. 6); otherwise it is resolved relative to this source file so `go run`/`go test` work
// from anywhere in the dev tree. This file is agents/go/internal/config/config.go, so
// agents/data is three directories up plus "data".
func dataDir() string {
	if dir := os.Getenv("AGENT_DATA_DIR"); dir != "" {
		return dir
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		return filepath.Join(filepath.Dir(file), "..", "..", "..", "data")
	}
	return filepath.Join("..", "data") // fallback: relative to the working directory
}
