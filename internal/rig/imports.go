package rig

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/remotesource"
)

func formatBoundImports(imports []config.BoundImport) string {
	parts := make([]string, 0, len(imports))
	for _, bound := range sortedBoundImports(imports) {
		part := bound.Binding
		if source := strings.TrimSpace(bound.Import.Source); source != "" {
			part += "=" + source
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// canonicalizeBuiltinPackIncludes rewrites --include tokens that name a
// bundled pack to its canonical remote source. Builtin packs compose from
// the user-global repo cache and are not registered in [packs], so a bare
// "<name>" or "packs/<name>" token (the form documented in `gc rig add
// --help`) would otherwise be persisted as the non-resolvable literal
// "./<token>", breaking pack expansion citywide (gascity#3137). A token
// whose raw form or derived single-segment name is a key in packs, or
// that resolves to a real local pack directory in the city, is left
// unchanged so explicit references keep their configured/local source
// rather than being shadowed by the builtin.
func canonicalizeBuiltinPackIncludes(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource) []string {
	out := make([]string, len(includes))
	for i, inc := range includes {
		out[i] = inc
		tok := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(inc)), "./")
		name := tok
		if rest, ok := strings.CutPrefix(tok, "packs/"); ok {
			name = rest
		}
		// Only accept a single-segment pack name; arbitrary nested paths are
		// treated as real local imports, not builtin-pack references.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		// Don't shadow an explicitly configured [packs] reference: a token
		// that names a registered pack keeps its configured source.
		if _, ok := packs[tok]; ok {
			continue
		}
		if _, ok := packs[name]; ok {
			continue
		}
		// A token that resolves to a real local pack in the city is a local
		// import, not a builtin-pack reference.
		if !filepath.IsAbs(tok) {
			if _, err := fs.Stat(filepath.Join(cityPath, filepath.FromSlash(tok), "pack.toml")); err == nil {
				continue
			}
		}
		if source, ok := builtinpacks.CanonicalImportSource(name); ok {
			out[i] = source
		}
	}
	return out
}

// resolveIncludeSources canonicalizes the --include tokens for a rig add and
// fails when any token names something gc cannot resolve to a pack.
//
// It is the single entry point for Step 4 of Provision: canonicalization and
// validation must run as a pair, because canonicalization is what turns a
// bundled-pack name into a resolvable remote source, and validation is what
// catches every token it did not rewrite. Splitting them at the call site
// would let a future caller take the rewrite without the guard.
func resolveIncludeSources(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource) ([]string, error) {
	canonical := canonicalizeBuiltinPackIncludes(fs, cityPath, includes, packs)
	if err := validateIncludeSources(fs, cityPath, canonical, packs); err != nil {
		return nil, err
	}
	return canonical, nil
}

// validateIncludeSources rejects canonicalized --include tokens that do not
// resolve to a pack, so `gc rig add` fails at add time instead of writing an
// unresolvable rig import (gascity#4620).
//
// An unrecognized token degrades to the literal local source "./<token>"
// (config.legacyImportSourceFor), which the pack loader resolves against the
// CITY ROOT. When no such directory exists, pack expansion fails for every
// rig in the city rather than only the rig being added, and the failure
// surfaces far from the command that caused it — `gc rig add` has already
// reported success by then. Every token is checked through one classifier so
// resolution is consistent across names: the pre-fix behavior rewrote a
// bundled name like "gastown" to a canonical URL while a sibling name
// silently became "./<name>".
func validateIncludeSources(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource) error {
	var unresolved []string
	for _, inc := range includes {
		if token := strings.TrimSpace(inc); !includeSourceResolves(fs, cityPath, token, packs) {
			unresolved = append(unresolved, token)
		}
	}
	if len(unresolved) == 0 {
		return nil
	}
	bundled := strings.Join(bundledPackNames(), ", ")
	lines := make([]string, 0, len(unresolved))
	for _, token := range unresolved {
		lines = append(lines, fmt.Sprintf("--include %q does not resolve to a pack.\n"+
			"  Checked: bundled packs (%s), [packs] keys in city.toml, and %s.\n"+
			"  Pass a directory containing pack.toml, a remote source (https://…, github.com/owner/repo, git@…), or register the pack under [packs] in city.toml first.",
			token, bundled, filepath.Join(localIncludeDir(cityPath, token), "pack.toml")))
	}
	return errors.New(strings.Join(lines, "\n"))
}

// includeSourceResolves reports whether a canonicalized --include token names
// a pack gc can resolve. The checks mirror how the pack loader resolves a rig
// import source: registered [packs] keys win, remote sources are resolved
// later by the pack installer, and everything else must be a local directory
// holding pack.toml.
func includeSourceResolves(fs fsys.FS, cityPath, token string, packs map[string]config.PackSource) bool {
	if token == "" {
		return false
	}
	if _, ok := packs[token]; ok {
		return true
	}
	if _, ok := packs[includeBindingHint(token)]; ok {
		return true
	}
	if remotesource.IsRemote(token) {
		return true
	}
	_, err := fs.Stat(filepath.Join(localIncludeDir(cityPath, token), "pack.toml"))
	return err == nil
}

// localIncludeDir resolves a local --include token to the directory the pack
// loader would read it from. Rig imports are declared in city.toml, so
// relative sources resolve against the city root (config.resolveConfigPath
// with declDir == cityRoot) — not against the working directory the operator
// happened to run `gc rig add` from.
func localIncludeDir(cityPath, token string) string {
	slash := filepath.ToSlash(token)
	if rest, ok := strings.CutPrefix(slash, "//"); ok {
		return filepath.Join(cityPath, filepath.FromSlash(rest))
	}
	if filepath.IsAbs(token) {
		return token
	}
	return filepath.Join(cityPath, filepath.FromSlash(slash))
}

// includeBindingHint reduces an --include token to the single-segment pack
// name it would bind as, matching canonicalizeBuiltinPackIncludes so the
// [packs] lookup and the error message agree on what the operator named.
func includeBindingHint(token string) string {
	name := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(token)), "./")
	if rest, ok := strings.CutPrefix(name, "packs/"); ok {
		name = rest
	}
	return name
}

// bundledPackNames lists the packs gc can resolve from a bare name, for the
// unresolvable-include error. Reading the registry keeps the message correct
// as packs are added rather than restating a list that drifts.
func bundledPackNames() []string {
	all := builtinpacks.All()
	names := make([]string, 0, len(all))
	for _, pack := range all {
		names = append(names, pack.Name)
	}
	return names
}

func boundImportsFromImportMap(imports map[string]config.Import) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	bindings := make([]string, 0, len(imports))
	for binding := range imports {
		bindings = append(bindings, binding)
	}
	slices.Sort(bindings)
	bound := make([]config.BoundImport, 0, len(bindings))
	for _, binding := range bindings {
		bound = append(bound, config.BoundImport{
			Binding: binding,
			Import:  imports[binding],
		})
	}
	return bound
}

func effectiveRigBoundImports(rig *config.Rig, packs map[string]config.PackSource) ([]config.BoundImport, error) {
	if rig == nil {
		return nil, nil
	}
	legacy := config.BoundImportsFromLegacySources(rig.Includes, packs)
	return MergeBoundImports(boundImportsFromImportMap(rig.Imports), legacy)
}

func composeDefaultRigImports(root []config.BoundImport, legacyIncludes []string, packs map[string]config.PackSource) []config.BoundImport {
	if len(root) == 0 {
		return config.BoundImportsFromLegacySources(legacyIncludes, packs)
	}
	target := make(map[string]config.Import, len(root)+len(legacyIncludes))
	order := make([]string, 0, len(root)+len(legacyIncludes))
	for _, bound := range root {
		if _, exists := target[bound.Binding]; !exists {
			order = append(order, bound.Binding)
		}
		target[bound.Binding] = bound.Import
	}
	order, _ = config.AddOrderedLegacyImports(target, order, legacyIncludes, packs)
	out := make([]config.BoundImport, 0, len(order))
	for _, binding := range order {
		imp, ok := target[binding]
		if !ok {
			continue
		}
		out = append(out, config.BoundImport{Binding: binding, Import: imp})
	}
	return out
}

func sortedBoundImports(imports []config.BoundImport) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	sorted := append([]config.BoundImport(nil), imports...)
	slices.SortFunc(sorted, func(a, b config.BoundImport) int {
		if a.Binding != b.Binding {
			return strings.Compare(a.Binding, b.Binding)
		}
		return strings.Compare(a.Import.Source, b.Import.Source)
	})
	return sorted
}

// MergeBoundImports is for already-bound import sets. Legacy default-rig
// includes use composeDefaultRigImports so binding collisions can be
// uniquified with the migration policy.
func MergeBoundImports(primary, secondary []config.BoundImport) ([]config.BoundImport, error) {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil, nil
	}
	merged := make([]config.BoundImport, 0, len(primary)+len(secondary))
	seenByBinding := make(map[string]config.Import, len(primary)+len(secondary))
	appendImport := func(bound config.BoundImport) error {
		if prior, exists := seenByBinding[bound.Binding]; exists {
			if prior.Source == bound.Import.Source {
				return nil
			}
			return fmt.Errorf("binding %q maps to both %q and %q", bound.Binding, prior.Source, bound.Import.Source)
		}
		seenByBinding[bound.Binding] = bound.Import
		merged = append(merged, bound)
		return nil
	}
	for _, bound := range primary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	for _, bound := range secondary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	return sortedBoundImports(merged), nil
}

func boundImportsMap(imports []config.BoundImport) map[string]config.Import {
	if len(imports) == 0 {
		return nil
	}
	out := make(map[string]config.Import, len(imports))
	for _, bound := range imports {
		out[bound.Binding] = bound.Import
	}
	return out
}
