package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	serviceRowPattern  = regexp.MustCompile(`(?m)^\s*\(\s*'([^']+)'\s*,`)
	incidentRowPattern = regexp.MustCompile(
		`(?ms)^\s*\(\s*'([^']+)'\s*,\s*'([^']+)'\s*,\s*'[^']*'\s*,\s*'([^']+)'\s*,\s*'[^']*'\s*,\s*'([^']+)'`,
	)
)

type Incident struct {
	ID       string
	Service  string
	Severity string
	Runbook  string
}

type Domain struct {
	Services  map[string]struct{}
	Runbooks  map[string]struct{}
	Skills    map[string]struct{}
	Incidents map[string]Incident
}

func LoadDomain(dataDir string) (Domain, error) {
	seedPath := filepath.Join(dataDir, "sql", "seed.sql")
	seed, err := os.ReadFile(seedPath)
	if err != nil {
		return Domain{}, fmt.Errorf("read domain seed %s: %w", seedPath, err)
	}
	servicesBlock, err := sqlInsertBlock(string(seed), "services")
	if err != nil {
		return Domain{}, err
	}
	services := make(map[string]struct{})
	for _, match := range serviceRowPattern.FindAllStringSubmatch(servicesBlock, -1) {
		services[match[1]] = struct{}{}
	}
	incidentsBlock, err := sqlInsertBlock(string(seed), "incidents")
	if err != nil {
		return Domain{}, err
	}
	incidents := make(map[string]Incident)
	for _, match := range incidentRowPattern.FindAllStringSubmatch(incidentsBlock, -1) {
		incidents[match[1]] = Incident{
			ID:       match[1],
			Service:  match[2],
			Severity: match[3],
			Runbook:  match[4],
		}
	}
	runbooks, err := slugsWithExtension(filepath.Join(dataDir, "runbooks"), ".md")
	if err != nil {
		return Domain{}, err
	}
	skills, err := skillSlugs(filepath.Join(dataDir, "skills"))
	if err != nil {
		return Domain{}, err
	}
	if len(services) == 0 || len(incidents) == 0 || len(runbooks) == 0 {
		return Domain{}, errorsForEmptyDomain(len(services), len(incidents), len(runbooks))
	}
	return Domain{Services: services, Runbooks: runbooks, Skills: skills, Incidents: incidents}, nil
}

func sqlInsertBlock(seed, table string) (string, error) {
	marker := "INSERT INTO " + table
	start := strings.Index(seed, marker)
	if start < 0 {
		return "", fmt.Errorf("domain seed has no %s insert", table)
	}
	remainder := seed[start:]
	end := regexp.MustCompile(`(?m);\s*$`).FindStringIndex(remainder)
	if end == nil {
		return "", fmt.Errorf("domain seed has an unterminated %s insert", table)
	}
	return remainder[:end[0]], nil
}

func slugsWithExtension(directory, extension string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read domain directory %s: %w", directory, err)
	}
	slugs := make(map[string]struct{})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != extension {
			continue
		}
		slugs[strings.TrimSuffix(entry.Name(), extension)] = struct{}{}
	}
	return slugs, nil
}

func skillSlugs(directory string) (map[string]struct{}, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read domain skills %s: %w", directory, err)
	}
	slugs := make(map[string]struct{})
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(directory, entry.Name(), "SKILL.md")); err == nil {
			slugs[entry.Name()] = struct{}{}
		}
	}
	return slugs, nil
}

func errorsForEmptyDomain(services, incidents, runbooks int) error {
	return fmt.Errorf(
		"domain seed is incomplete: services=%d incidents=%d runbooks=%d",
		services,
		incidents,
		runbooks,
	)
}
