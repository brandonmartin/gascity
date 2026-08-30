package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureClaudeProjectTrustedSeedsMissingFile(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()

	if err := EnsureClaudeProjectTrusted(home, "", work); err != nil {
		t.Fatalf("EnsureClaudeProjectTrusted: %v", err)
	}

	got := readClaudeState(t, filepath.Join(home, ".claude.json"))
	entry := projectEntry(t, got, work)
	if entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("hasTrustDialogAccepted not set: %v", entry)
	}
	if entry["hasCompletedProjectOnboarding"] != true {
		t.Fatalf("hasCompletedProjectOnboarding not set: %v", entry)
	}
	// projectOnboardingSeenCount is written as JSON number; unmarshal gives float64.
	if v, _ := entry["projectOnboardingSeenCount"].(float64); v != 1 {
		t.Fatalf("projectOnboardingSeenCount = %v, want 1", entry["projectOnboardingSeenCount"])
	}
}

func TestEnsureClaudeProjectTrustedPreservesOtherFields(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	statePath := filepath.Join(home, ".claude.json")

	initial := map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
		"userID":                 "user-42",
		"projects": map[string]any{
			"/some/other/project": map[string]any{
				"hasTrustDialogAccepted": true,
				"customField":            "keep-me",
			},
		},
	}
	writeClaudeState(t, statePath, initial)

	if err := EnsureClaudeProjectTrusted(home, "", work); err != nil {
		t.Fatalf("EnsureClaudeProjectTrusted: %v", err)
	}

	got := readClaudeState(t, statePath)
	if got["hasCompletedOnboarding"] != true {
		t.Fatalf("hasCompletedOnboarding overwritten: %v", got)
	}
	if got["theme"] != "dark" {
		t.Fatalf("theme overwritten: %v", got["theme"])
	}
	if got["userID"] != "user-42" {
		t.Fatalf("userID overwritten: %v", got["userID"])
	}
	other := projectEntry(t, got, "/some/other/project")
	if other["customField"] != "keep-me" {
		t.Fatalf("existing project entry lost custom field: %v", other)
	}
	entry := projectEntry(t, got, work)
	if entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("new project not trusted: %v", entry)
	}
}

func TestEnsureClaudeProjectTrustedIdempotent(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	statePath := filepath.Join(home, ".claude.json")

	if err := EnsureClaudeProjectTrusted(home, "", work); err != nil {
		t.Fatalf("first call: %v", err)
	}
	fi1, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Second call must be a no-op — no rewrite, so mtime is unchanged.
	if err := EnsureClaudeProjectTrusted(home, "", work); err != nil {
		t.Fatalf("second call: %v", err)
	}
	fi2, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatalf("idempotent call rewrote file (mtime %v -> %v)", fi1.ModTime(), fi2.ModTime())
	}
}

func TestEnsureClaudeProjectTrustedWritesBothLocations(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	work := t.TempDir()

	if err := EnsureClaudeProjectTrusted(home, configDir, work); err != nil {
		t.Fatalf("EnsureClaudeProjectTrusted: %v", err)
	}

	for _, p := range []string{filepath.Join(home, ".claude.json"), filepath.Join(configDir, ".claude.json")} {
		got := readClaudeState(t, p)
		entry := projectEntry(t, got, work)
		if entry["hasTrustDialogAccepted"] != true {
			t.Fatalf("%s: not trusted: %v", p, entry)
		}
	}
}

func TestEnsureClaudeProjectTrustedNoopOnEmptyInputs(t *testing.T) {
	if err := EnsureClaudeProjectTrusted("", "", "/anything"); err != nil {
		t.Fatalf("empty home: %v", err)
	}
	home := t.TempDir()
	if err := EnsureClaudeProjectTrusted(home, "", ""); err != nil {
		t.Fatalf("empty project: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("empty project should not create state file, got err=%v", err)
	}
}

func TestEnsureClaudeProjectTrustedResolvesRelativePath(t *testing.T) {
	home := t.TempDir()
	// Use "." so the abs path resolves to the test's cwd — a stable, known abs path.
	if err := EnsureClaudeProjectTrusted(home, "", "."); err != nil {
		t.Fatalf("EnsureClaudeProjectTrusted: %v", err)
	}
	abs, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	got := readClaudeState(t, filepath.Join(home, ".claude.json"))
	entry := projectEntry(t, got, abs)
	if entry["hasTrustDialogAccepted"] != true {
		t.Fatalf("relative path not resolved: keys=%v", projectKeys(got))
	}
}

func TestEnsureClaudeProjectTrustedRejectsMalformedFile(t *testing.T) {
	home := t.TempDir()
	work := t.TempDir()
	statePath := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(statePath, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write malformed: %v", err)
	}
	if err := EnsureClaudeProjectTrusted(home, "", work); err == nil {
		t.Fatalf("expected error on malformed file, got nil")
	}
	// Original bytes must remain untouched — we don't clobber user state.
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(data) != "not-json" {
		t.Fatalf("malformed file was overwritten: %q", string(data))
	}
}

func readClaudeState(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}

func writeClaudeState(t *testing.T, path string, root map[string]any) {
	t.Helper()
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func projectEntry(t *testing.T, root map[string]any, projectPath string) map[string]any {
	t.Helper()
	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		t.Fatalf("projects map missing; keys=%v", mapKeys(root))
	}
	entry, _ := projects[projectPath].(map[string]any)
	if entry == nil {
		t.Fatalf("project %q missing; have %v", projectPath, projectKeys(root))
	}
	return entry
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func projectKeys(root map[string]any) []string {
	projects, _ := root["projects"].(map[string]any)
	return mapKeys(projects)
}
