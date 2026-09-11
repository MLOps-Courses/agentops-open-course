package conventions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckSkillsRejectsNameAndPortabilityDrift(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "skills", "portable")
	if err := os.MkdirAll(directory, 0o750); err != nil {
		t.Fatal(err)
	}
	text := "---\nname: wrong\ndescription: x\n---\n\n# Skill\n\nUse /home/person/file.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	messages := problemMessages(CheckSkills(root))
	if !strings.Contains(messages, "must match its directory") || !strings.Contains(messages, "machine-specific") {
		t.Fatal(messages)
	}
}
