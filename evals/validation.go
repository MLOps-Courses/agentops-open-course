package evals

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const forbiddenAgentImportPrefix = "github.com/MLOps-Courses/agentops-open-course/agents/go"

type AssetPaths struct {
	ModuleDir   string
	DataDir     string
	Calibration string
	Dashboard   string
	EvalSets    []string
}

type ValidationSummary struct {
	EvalSets         int `json:"evalsets"`
	Cases            int `json:"cases"`
	CalibrationCases int `json:"calibration_cases"`
}

func ValidateAssets(ctx context.Context, paths AssetPaths) (ValidationSummary, error) {
	domain, err := LoadDomain(paths.DataDir)
	if err != nil {
		return ValidationSummary{}, err
	}
	summary := ValidationSummary{}
	for _, path := range paths.EvalSets {
		evalset, loadErr := LoadEvalSet(path)
		if loadErr != nil {
			return ValidationSummary{}, loadErr
		}
		if domainErr := evalset.ValidateDomain(domain); domainErr != nil {
			return ValidationSummary{}, fmt.Errorf("validate evalset domain %s: %w", path, domainErr)
		}
		summary.EvalSets++
		summary.Cases += len(evalset.Cases)
		if strings.Contains(filepath.Base(path), "triage-report") {
			for _, evalCase := range evalset.Cases {
				for _, invocation := range evalCase.Conversation {
					if _, reportErr := ParseTriageReport(invocation.FinalResponse.Text()); reportErr != nil {
						return ValidationSummary{}, fmt.Errorf("reference report %q: %w", evalCase.ID, reportErr)
					}
				}
			}
		}
	}
	calibration, err := LoadCalibrationSet(paths.Calibration)
	if err != nil {
		return ValidationSummary{}, err
	}
	summary.CalibrationCases = len(calibration.Cases)
	if err := validateDashboard(paths.Dashboard); err != nil {
		return ValidationSummary{}, err
	}
	if err := ValidateImportBoundary(ctx, paths.ModuleDir); err != nil {
		return ValidationSummary{}, err
	}
	return summary, nil
}

func ValidateImportBoundary(ctx context.Context, moduleDir string) error {
	goMod, err := os.ReadFile(filepath.Join(moduleDir, "go.mod"))
	if err != nil {
		return fmt.Errorf("read evaluation go.mod: %w", err)
	}
	if strings.Contains(string(goMod), forbiddenAgentImportPrefix) {
		return errors.New("evaluation go.mod requires the agent module")
	}
	command := exec.CommandContext(ctx, "go", "list", "-deps", "-test", "./...")
	command.Dir = moduleDir
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("resolve evaluation import graph: %w", err)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		if scanner.Text() == forbiddenAgentImportPrefix || strings.HasPrefix(scanner.Text(), forbiddenAgentImportPrefix+"/") {
			return fmt.Errorf("evaluation import graph crosses into %s", scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan evaluation import graph: %w", err)
	}
	return nil
}

func validateDashboard(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open evaluation dashboard %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	var dashboard struct {
		UID    string            `json:"uid"`
		Title  string            `json:"title"`
		Panels []json.RawMessage `json:"panels"`
	}
	if err := decoder.Decode(&dashboard); err != nil {
		return fmt.Errorf("decode evaluation dashboard %s: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return fmt.Errorf("decode evaluation dashboard %s: %w", path, err)
	}
	if dashboard.UID == "" || dashboard.Title == "" || len(dashboard.Panels) < 2 {
		return errors.New("evaluation dashboard needs a uid, title, and at least two panels")
	}
	return nil
}
