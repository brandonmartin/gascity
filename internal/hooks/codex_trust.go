package hooks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
)

// codexHookTrustEntry is one Codex [hooks.state."<key>"] trusted_hash row.
// The key is "<abs hooks.json>:<event>:<group>:<handler>", matching Codex's
// hook_key. The hash is Codex's version_for_toml of the normalized hook
// identity (sha256 of canonical JSON).
type codexHookTrustEntry struct {
	Key  string
	Hash string
}

const (
	codexHookDefaultTimeoutSec    = 600
	codexSessionEndDefaultTimeout = 1
	codexSessionEndMaxTimeout     = 3
)

// codexUserConfigPath is where Codex persists hook trust. Tests leave it
// empty unless GC_CODEX_HOOK_TRUST_CONFIG points at a fixture, so hook
// installs under go test never rewrite a developer's real config.toml.
func codexUserConfigPath() string {
	if override := strings.TrimSpace(os.Getenv("GC_CODEX_HOOK_TRUST_CONFIG")); override != "" {
		return override
	}
	if testing.Testing() || os.Getenv("GC_CODEX_HOOK_TRUST") == "off" {
		return ""
	}
	home := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return ""
		}
		home = filepath.Join(home, ".codex")
	}
	return filepath.Join(home, "config.toml")
}

func pretrustInstalledCodexHooks(hooksPath string, data []byte) error {
	configPath := codexUserConfigPath()
	if configPath == "" || !filepath.IsAbs(hooksPath) {
		return nil
	}
	if _, err := os.Stat(hooksPath); err != nil {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parsing %s for hook trust: %w", hooksPath, err)
	}
	if !codexHookDocLooksManaged(root) {
		return nil
	}
	return PretrustCodexHooks(configPath, hooksPath, data)
}

// PretrustCodexHooks writes trusted_hash entries for every command hook in
// data so a later Codex start does not block on "Hooks need review". Existing
// config.toml tables, comments, and enabled flags are preserved.
func PretrustCodexHooks(configPath, hooksPath string, data []byte) error {
	abs, err := filepath.Abs(hooksPath)
	if err != nil {
		return fmt.Errorf("resolving hooks path: %w", err)
	}
	entries, err := codexHookTrustEntries(abs, data)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	return upsertCodexHookTrustFile(configPath, entries)
}

type codexTrustFile struct {
	Hooks map[string][]codexTrustGroup `json:"hooks"`
}

type codexTrustGroup struct {
	Matcher *string             `json:"matcher"`
	Hooks   []codexTrustHandler `json:"hooks"`
}

type codexTrustHandler struct {
	Type          string `json:"type"`
	Command       string `json:"command"`
	Timeout       *int   `json:"timeout"`
	Async         bool   `json:"async"`
	StatusMessage string `json:"statusMessage"`
}

func codexHookTrustEntries(hooksPath string, data []byte) ([]codexHookTrustEntry, error) {
	var doc codexTrustFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing hooks for trust: %w", err)
	}
	var entries []codexHookTrustEntry
	for event, groups := range doc.Hooks {
		eventKey := codexHookEventKey(event)
		for gi, group := range groups {
			for hi, handler := range group.Hooks {
				if handler.Type != "command" || strings.TrimSpace(handler.Command) == "" {
					continue
				}
				hash, err := codexHookTrustHash(eventKey, codexTrustMatcher(eventKey, group.Matcher), handler)
				if err != nil {
					return nil, err
				}
				entries = append(entries, codexHookTrustEntry{
					Key:  fmt.Sprintf("%s:%s:%d:%d", hooksPath, eventKey, gi, hi),
					Hash: hash,
				})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

func codexHookEventKey(event string) string {
	var b strings.Builder
	for i, r := range event {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToLower(b.String())
}

func codexTrustMatcher(event string, matcher *string) *string {
	switch event {
	case "user_prompt_submit", "stop", "interrupt":
		return nil
	default:
		return matcher
	}
}

func codexHookTrustHash(event string, matcher *string, handler codexTrustHandler) (string, error) {
	hook := map[string]any{
		"async":   handler.Async,
		"command": handler.Command,
		"timeout": codexTrustTimeout(event, handler.Timeout),
		"type":    "command",
	}
	if msg := strings.TrimSpace(handler.StatusMessage); msg != "" {
		hook["statusMessage"] = msg
	}
	ident := map[string]any{
		"event_name": event,
		"hooks":      []any{hook},
	}
	if matcher != nil {
		ident["matcher"] = *matcher
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(ident); err != nil {
		return "", fmt.Errorf("encoding hook trust identity: %w", err)
	}
	sum := sha256.Sum256(bytes.TrimRight(buf.Bytes(), "\n"))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func codexTrustTimeout(event string, timeout *int) int {
	if event == "session_end" || event == "interrupt" {
		if timeout == nil {
			return codexSessionEndDefaultTimeout
		}
		v := *timeout
		if v < 1 {
			return 1
		}
		if v > codexSessionEndMaxTimeout {
			return codexSessionEndMaxTimeout
		}
		return v
	}
	if timeout == nil {
		return codexHookDefaultTimeoutSec
	}
	if *timeout < 1 {
		return 1
	}
	return *timeout
}

func upsertCodexHookTrustFile(configPath string, entries []codexHookTrustEntry) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("creating codex config dir: %w", err)
	}
	lock, err := os.OpenFile(configPath+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening codex config lock: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking codex config: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()

	existing, err := os.ReadFile(configPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reading codex config: %w", err)
	}
	updated, changed := mergeCodexHookTrust(string(existing), entries)
	if !changed {
		return nil
	}
	return writeAtomicFile(configPath, []byte(updated))
}

func writeAtomicFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".codex-config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating codex config temp: %w", err)
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("writing codex config temp: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("closing codex config temp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replacing codex config: %w", err)
	}
	return nil
}

var trustedHashLine = regexp.MustCompile(`^(\s*)trusted_hash\s*=\s*(.+?)\s*$`)

func mergeCodexHookTrust(content string, entries []codexHookTrustEntry) (string, bool) {
	newline := "\n"
	if strings.Contains(content, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(content, newline)
	changed := false
	remaining := append([]codexHookTrustEntry(nil), entries...)

	for i := 0; i < len(lines); i++ {
		key, ok := hooksStateSectionKey(lines[i])
		if !ok {
			continue
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(strings.TrimSpace(lines[j]), "[") {
				end = j
				break
			}
		}
		idx := trustEntryIndex(remaining, key)
		if idx < 0 {
			i = end - 1
			continue
		}
		want := remaining[idx].Hash
		remaining = append(remaining[:idx], remaining[idx+1:]...)
		replaced := false
		for k := i + 1; k < end; k++ {
			m := trustedHashLine.FindStringSubmatch(lines[k])
			if m == nil {
				continue
			}
			if got, ok := unquoteTOMLValue(m[2]); ok && got == want {
				replaced = true
				break
			}
			lines[k] = m[1] + "trusted_hash = " + quoteTOML(want)
			changed = true
			replaced = true
			break
		}
		if !replaced {
			insert := "trusted_hash = " + quoteTOML(want)
			lines = insertLine(lines, end, insert)
			changed = true
			end++
		}
		i = end - 1
	}

	body := joinLines(lines, newline)
	if len(remaining) == 0 {
		return body, changed
	}
	var b strings.Builder
	b.WriteString(body)
	if b.Len() > 0 && !strings.HasSuffix(b.String(), newline) {
		b.WriteString(newline)
	}
	for _, entry := range remaining {
		if b.Len() > 0 && !strings.HasSuffix(b.String(), newline+newline) {
			b.WriteString(newline)
		}
		fmt.Fprintf(&b, "[hooks.state.%s]%strusted_hash = %s%s", quoteTOML(entry.Key), newline, quoteTOML(entry.Hash), newline)
	}
	return b.String(), true
}

func trustEntryIndex(entries []codexHookTrustEntry, key string) int {
	for i, entry := range entries {
		if entry.Key == key {
			return i
		}
	}
	return -1
}

func insertLine(lines []string, at int, line string) []string {
	if at > len(lines) {
		at = len(lines)
	}
	lines = append(lines, "")
	copy(lines[at+1:], lines[at:])
	lines[at] = line
	return lines
}

func joinLines(lines []string, newline string) string {
	return strings.Join(lines, newline)
}

func hooksStateSectionKey(line string) (string, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "[") {
		return "", false
	}
	end := strings.Index(s, "]")
	if end < 0 {
		return "", false
	}
	body := strings.TrimSpace(s[1:end])
	const prefix = "hooks.state."
	if !strings.HasPrefix(body, prefix) {
		return "", false
	}
	return unquoteTOMLValue(strings.TrimSpace(body[len(prefix):]))
}

func quoteTOML(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

func unquoteTOMLValue(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return "", false
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch s[i] {
			case '\\':
				if i+1 >= len(s) {
					return "", false
				}
				b.WriteByte(s[i+1])
				i++
			case '"':
				return b.String(), true
			default:
				b.WriteByte(s[i])
			}
		}
		return "", false
	case '\'':
		end := strings.LastIndex(s, "'")
		if end <= 0 {
			return "", false
		}
		return s[1:end], true
	default:
		return "", false
	}
}
