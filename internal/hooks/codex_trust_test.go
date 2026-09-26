package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCodexHookTrustHashMatchesCodex0157(t *testing.T) {
	t.Parallel()
	startup := "startup"
	empty := ""
	cases := []struct {
		event   string
		matcher *string
		command string
		want    string
	}{
		{
			event:   "session_start",
			matcher: &startup,
			command: `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" --city '/home/b/gc/boomfartville' prime --hook --hook-format codex`,
			want:    "sha256:80e2ae44830dfb2b1ddc01649562efea18a635eb5e13460d5825f21d4221e98e",
		},
		{
			event:   "session_start",
			matcher: &empty,
			command: `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && GC_MANAGED_SESSION_HOOK=1 GC_HOOK_EVENT_NAME=SessionStart "${GC_BIN:-gc}" prime --hook --hook-format codex`,
			want:    "sha256:8f0a1a7bc2783a8b23c56f3a52924b9bf5afb353c87b28b2cad9cb5e0393b4cb",
		},
		{
			event:   "user_prompt_submit",
			command: `export PATH="$PATH:$HOME/go/bin:$HOME/.local/bin" && "${GC_BIN:-gc}" hook run --timeout 15s --timeout-exit-code 0 -- nudge drain --inject --hook-format codex`,
			want:    "sha256:3260a3ade74575354763aaceda14d130905d521d1fff2c01f2da07de012d2688",
		},
	}
	for _, tc := range cases {
		t.Run(tc.event, func(t *testing.T) {
			got, err := codexHookTrustHash(tc.event, tc.matcher, codexTrustHandler{Type: "command", Command: tc.command})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("hash = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCodexHookTrustEntriesDropPromptMatcher(t *testing.T) {
	t.Parallel()
	const path = "/work/.codex/hooks.json"
	data := []byte(`{"hooks":{"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"echo hi"}]}]}}`)
	entries, err := codexHookTrustEntries(path, data)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	if entries[0].Key != path+":user_prompt_submit:0:0" {
		t.Fatalf("key = %s", entries[0].Key)
	}
	withMatcher, err := codexHookTrustHash("user_prompt_submit", strPtr(""), codexTrustHandler{Command: "echo hi"})
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Hash == withMatcher {
		t.Fatal("user_prompt_submit hash included an empty matcher")
	}
}

func TestMergeCodexHookTrustPreservesConfig(t *testing.T) {
	t.Parallel()
	in := "# keep\nmodel = \"gpt\"\n\n[hooks.state]\n\n[hooks.state.\"/old:session_start:0:0\"]\nenabled = false\ntrusted_hash = \"sha256:stale\"\n"
	entries := []codexHookTrustEntry{
		{Key: "/old:session_start:0:0", Hash: "sha256:fresh"},
		{Key: "/new:pre_compact:0:0", Hash: "sha256:new"},
	}
	got, changed := mergeCodexHookTrust(in, entries)
	if !changed {
		t.Fatal("expected a change")
	}
	for _, want := range []string{
		"# keep",
		`model = "gpt"`,
		"enabled = false",
		`trusted_hash = "sha256:fresh"`,
		`[hooks.state."/new:pre_compact:0:0"]`,
		`trusted_hash = "sha256:new"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "sha256:stale") {
		t.Fatalf("stale hash survived:\n%s", got)
	}
	again, changed := mergeCodexHookTrust(got, entries)
	if changed {
		t.Fatalf("second merge changed the file:\n%s", again)
	}
}

func TestInstallCodexPretrustsManagedHooks(t *testing.T) {
	cfgDir := t.TempDir()
	cfg := filepath.Join(cfgDir, "config.toml")
	if err := os.WriteFile(cfg, []byte("model = \"gpt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codexHookTrustConfigOverride = cfg
	t.Cleanup(func() { codexHookTrustConfigOverride = "" })
	work := t.TempDir()
	if err := Install(fsys.OSFS{}, "/city", work, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, `model = "gpt"`) {
		t.Fatalf("config lost unrelated keys:\n%s", text)
	}
	if !strings.Contains(text, "trusted_hash = \"sha256:") {
		t.Fatalf("config missing hook trust:\n%s", text)
	}
	hooksPath := filepath.Join(work, ".codex", "hooks.json")
	if !strings.Contains(text, hooksPath) {
		t.Fatalf("trust key does not name %s\n%s", hooksPath, text)
	}
	if err := Install(fsys.OSFS{}, "/city", work, []string{"codex"}); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != text {
		t.Fatalf("second install rewrote trust config\nbefore:\n%s\nafter:\n%s", text, again)
	}
}

func strPtr(s string) *string { return &s }
