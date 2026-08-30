package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnsureClaudeProjectTrusted marks projectPath as trusted in the caller-visible
// Claude Code state file(s) so Claude's first-run folder-trust dialog does not
// appear on session start.
//
// Claude Code renders a "Do you trust the files in this folder?" modal the very
// first time it opens a workdir it has not seen before, and its default option
// is "No, exit" — which makes the pane die exit 1. gc's dialog helpers observe
// the pane too late in some races (waking a long-dormant seat, a fresh HOME
// after a container recycle), and even when they don't the safer fix is to
// avoid the modal entirely: Claude persists per-project trust in ~/.claude.json
// under projects[<abs workdir>].hasTrustDialogAccepted, so seeding that entry
// before launch makes the modal never render. See ga-1e7.
//
// Both HOME/.claude.json and (when set) CLAUDE_CONFIG_DIR/.claude.json are
// updated, matching Claude Code's own dual-location discovery. Existing fields
// are preserved: the file is loaded, mutated, and re-written. Missing files
// are created with permissions 0600. Empty homeDir or projectPath is a no-op.
// A non-absolute projectPath is resolved to an absolute path before writing so
// the key matches what Claude Code itself records.
func EnsureClaudeProjectTrusted(homeDir, configDir, projectPath string) error {
	homeDir = strings.TrimSpace(homeDir)
	projectPath = strings.TrimSpace(projectPath)
	if homeDir == "" || projectPath == "" {
		return nil
	}
	if !filepath.IsAbs(projectPath) {
		abs, err := filepath.Abs(projectPath)
		if err != nil {
			return fmt.Errorf("resolving project path %q: %w", projectPath, err)
		}
		projectPath = abs
	}
	for _, path := range claudeTrustStatePaths(homeDir, configDir) {
		if err := seedClaudeProjectTrust(path, projectPath); err != nil {
			return fmt.Errorf("seeding claude trust %s: %w", path, err)
		}
	}
	return nil
}

func claudeTrustStatePaths(home, configDir string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	add(filepath.Join(home, ".claude.json"))
	if strings.TrimSpace(configDir) != "" {
		add(filepath.Join(configDir, ".claude.json"))
	}
	return out
}

func seedClaudeProjectTrust(path, projectPath string) error {
	root, err := loadClaudeTrustState(path)
	if err != nil {
		return err
	}
	projects, _ := root["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	entry, _ := projects[projectPath].(map[string]any)
	if entry == nil {
		entry = map[string]any{}
	}
	if trusted, _ := entry["hasTrustDialogAccepted"].(bool); trusted {
		// Nothing to do; avoid a write (and its mtime) when the state is
		// already correct so we don't churn the file on every start.
		return nil
	}
	entry["hasTrustDialogAccepted"] = true
	if _, ok := entry["hasCompletedProjectOnboarding"]; !ok {
		entry["hasCompletedProjectOnboarding"] = true
	}
	if _, ok := entry["projectOnboardingSeenCount"]; !ok {
		entry["projectOnboardingSeenCount"] = 1
	}
	projects[projectPath] = entry
	return saveClaudeTrustState(path, root)
}

func loadClaudeTrustState(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		// A malformed ~/.claude.json is not gc's file to repair — leave it
		// alone rather than clobbering user state and let Claude decide.
		return nil, err
	}
	if root == nil {
		root = map[string]any{}
	}
	return root, nil
}

func saveClaudeTrustState(path string, root map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".gc-tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
