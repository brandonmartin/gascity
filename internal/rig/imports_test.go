package rig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// writePackDir materializes a minimal pack directory (a dir holding
// pack.toml) at dir, which is what the pack loader requires of any local
// import source.
func writePackDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"local\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolveIncludeSourcesRejectsUnresolvableName is the gascity#4620
// regression guard: a --include token that names nothing gc can resolve —
// not a bundled pack, not a [packs] key, no pack.toml on disk — must fail
// at rig-add time instead of degrading to the literal "./<name>" source.
//
// The literal form resolves against the CITY ROOT, where the directory does
// not exist, so persisting it breaks pack expansion for every rig in the
// city, not just the rig being added. Reporting success while writing it is
// the worst outcome: the operator gets no signal at the point of the
// mistake, and the resulting failure presents as a Dolt/connectivity problem.
func TestResolveIncludeSourcesRejectsUnresolvableName(t *testing.T) {
	cityPath := t.TempDir()

	_, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"koolkats"}, nil)
	if err == nil {
		t.Fatal("resolveIncludeSources accepted an unresolvable --include name; it must fail loudly at add time")
	}
	msg := err.Error()
	// The message must name the offending token and the path that was
	// checked, so the operator can act without re-deriving the resolution
	// rules.
	if !strings.Contains(msg, "koolkats") {
		t.Errorf("error does not name the offending token: %q", msg)
	}
	if !strings.Contains(msg, filepath.Join(cityPath, "koolkats")) {
		t.Errorf("error does not report the local path that was checked: %q", msg)
	}
}

// TestResolveIncludeSourcesReportsEveryUnresolvableToken guards the
// diagnosis cost: an operator who passed several bad --include names must
// see all of them in one failure, not fix-and-retry once per token.
func TestResolveIncludeSourcesReportsEveryUnresolvableToken(t *testing.T) {
	cityPath := t.TempDir()

	_, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"koolkats", "oversight-rig"}, nil)
	if err == nil {
		t.Fatal("expected an error for two unresolvable --include names")
	}
	for _, want := range []string{"koolkats", "oversight-rig"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits unresolvable token %q: %q", want, err.Error())
		}
	}
}

// TestResolveIncludeSourcesAcceptsResolvableForms pins the other half of the
// contract: every token gc CAN resolve keeps working, and each resolves
// through the same classifier so the outcome is consistent across names.
// The observed defect surface in gascity#4620 was the split where "gastown"
// canonicalized to a URL while sibling names silently degraded — these cases
// are the ones that must not start failing.
func TestResolveIncludeSourcesAcceptsResolvableForms(t *testing.T) {
	cityPath := t.TempDir()
	writePackDir(t, filepath.Join(cityPath, "localpack"))
	writePackDir(t, filepath.Join(cityPath, "packs", "nested"))
	absPack := filepath.Join(t.TempDir(), "abspack")
	writePackDir(t, absPack)

	bundled, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	packs := map[string]config.PackSource{
		"registered": {Source: "https://github.com/example/registered"},
	}

	cases := []struct {
		name    string
		include string
		want    string
	}{
		{"bundled builtin name", "gastown", bundled},
		{"bundled builtin under packs/", "packs/gastown", bundled},
		{"registered [packs] key", "registered", "registered"},
		{"local pack dir", "localpack", "localpack"},
		{"local pack dir with ./ prefix", "./localpack", "./localpack"},
		{"nested local pack dir", "packs/nested", "packs/nested"},
		{"absolute local pack dir", absPack, absPack},
		{"https remote source", "https://github.com/example/pack", "https://github.com/example/pack"},
		{"github shorthand remote source", "github.com/example/pack", "github.com/example/pack"},
		{"scp-style remote source", "git@github.com:example/pack.git", "git@github.com:example/pack.git"},
		{"remote source with subpath and ref", "https://github.com/example/pack//sub#v1", "https://github.com/example/pack//sub#v1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{tc.include}, packs)
			if err != nil {
				t.Fatalf("resolveIncludeSources(%q) errored: %v", tc.include, err)
			}
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("resolveIncludeSources(%q) = %v, want [%q]", tc.include, got, tc.want)
			}
		})
	}
}

// TestResolveIncludeSourcesPrefersLocalAndConfiguredOverBuiltin preserves the
// gascity#3137 precedence rules through the added validation: a token that
// names a registered [packs] key or a real local pack dir keeps that
// meaning and is never shadowed by a same-named bundled pack.
func TestResolveIncludeSourcesPrefersLocalAndConfiguredOverBuiltin(t *testing.T) {
	cityPath := t.TempDir()
	writePackDir(t, filepath.Join(cityPath, "gastown"))

	got, err := resolveIncludeSources(fsys.OSFS{}, cityPath, []string{"gastown"}, nil)
	if err != nil {
		t.Fatalf("resolveIncludeSources: %v", err)
	}
	if len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("local pack dir was shadowed by the builtin: got %v, want [\"gastown\"]", got)
	}

	packs := map[string]config.PackSource{"gastown": {Source: "https://github.com/example/gastown"}}
	got, err = resolveIncludeSources(fsys.OSFS{}, t.TempDir(), []string{"gastown"}, packs)
	if err != nil {
		t.Fatalf("resolveIncludeSources: %v", err)
	}
	if len(got) != 1 || got[0] != "gastown" {
		t.Fatalf("configured [packs] key was shadowed by the builtin: got %v, want [\"gastown\"]", got)
	}
}
