package config

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// fakeModelDiscoverer is the scripted config.ModelDiscoverer used by these
// tests: it records every request and returns a canned listing.
type fakeModelDiscoverer struct {
	requests []ModelDiscoveryRequest
	models   []DiscoveredModel
}

func (f *fakeModelDiscoverer) DiscoverModels(_ context.Context, req ModelDiscoveryRequest) []DiscoveredModel {
	f.requests = append(f.requests, req)
	return append([]DiscoveredModel(nil), f.models...)
}

func installFakeModelDiscoverer(t *testing.T, models ...DiscoveredModel) *fakeModelDiscoverer {
	t.Helper()
	fake := &fakeModelDiscoverer{models: models}
	t.Cleanup(SetModelDiscoverer(fake))
	return fake
}

func modelOptionOf(t *testing.T, schema []ProviderOption) ProviderOption {
	t.Helper()
	opt := findOption(schema, ModelOptionKey)
	if opt == nil {
		t.Fatal("schema has no model option")
	}
	return *opt
}

func openModelSchema() []ProviderOption {
	return []ProviderOption{
		{
			Key:   "permission_mode",
			Label: "Permission Mode",
			Type:  "select",
			Choices: []OptionChoice{
				{Value: "plan", Label: "Plan", FlagArgs: []string{"--permission-mode", "plan"}},
			},
		},
		{
			Key:   ModelOptionKey,
			Label: "Model",
			Type:  "select",
			Choices: []OptionChoice{
				{Value: "", Label: "Default"},
				// A curated alias whose FlagArgs expand to a different id: the
				// template would emit "opus" verbatim, so the curated entry must
				// keep winning on collision.
				{Value: "opus", Label: "Opus", FlagArgs: []string{"--model", "claude-opus-4-8"}, FlagAliases: [][]string{{"-m", "claude-opus-4-8"}}},
				{Value: "claude-opus-5", Label: "Opus 5 (canonical id)", FlagArgs: []string{"--model", "claude-opus-5"}},
			},
			FlagTemplate: []string{"--model", OptionValuePlaceholder},
		},
	}
}

func TestMergeDiscoveredModelChoicesCuratedWinsAndDiscoveredAppends(t *testing.T) {
	schema := openModelSchema()
	before := deepCopyProviderOptions(schema)

	merged := MergeDiscoveredModelChoices(schema, []DiscoveredModel{
		{ID: "claude-opus-5", Label: "Opus 5 (from binary)"}, // collision: curated label/flags stay
		{ID: "opus", Label: "opus"},                          // collision with an alias: expansion stays
		{ID: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M"},
		{ID: "claude-opus-5-5-high", Label: "duplicate"}, // dedupe within the listing
		{ID: "", Label: "no id"},                         // dropped
		{ID: "claude-fable-5-1-xhigh"},                   // label falls back to the id
	})

	// Purity: the input schema is untouched.
	if !reflect.DeepEqual(schema, before) {
		t.Fatalf("MergeDiscoveredModelChoices mutated its input schema")
	}
	// Non-model options are carried through unchanged.
	if !reflect.DeepEqual(merged[0], before[0]) {
		t.Fatalf("permission_mode option changed: %+v", merged[0])
	}

	model := modelOptionOf(t, merged)
	wantChoices := append(deepCopyOptionChoices(before[1].Choices),
		OptionChoice{Value: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M", FlagArgs: []string{"--model", "claude-opus-5-5-high"}},
		OptionChoice{Value: "claude-fable-5-1-xhigh", Label: "claude-fable-5-1-xhigh", FlagArgs: []string{"--model", "claude-fable-5-1-xhigh"}},
	)
	if !reflect.DeepEqual(model.Choices, wantChoices) {
		t.Fatalf("model choices =\n%+v\nwant\n%+v", model.Choices, wantChoices)
	}
	if !reflect.DeepEqual(model.FlagTemplate, before[1].FlagTemplate) {
		t.Fatalf("FlagTemplate changed: %v", model.FlagTemplate)
	}

	// A discovered id must resolve to flags exactly like a curated one, and
	// the open-enum passthrough for ids nobody listed must be untouched.
	if args, ok := OptionFlagArgs(model, "claude-opus-5-5-high"); !ok || !reflect.DeepEqual(args, []string{"--model", "claude-opus-5-5-high"}) {
		t.Fatalf("discovered id flags = %v, %v", args, ok)
	}
	if args, ok := OptionFlagArgs(model, "opus"); !ok || !reflect.DeepEqual(args, []string{"--model", "claude-opus-4-8"}) {
		t.Fatalf("curated alias flags = %v, %v; discovery must not override the alias expansion", args, ok)
	}
	if args, ok := OptionFlagArgs(model, "never-listed-anywhere"); !ok || !reflect.DeepEqual(args, []string{"--model", "never-listed-anywhere"}) {
		t.Fatalf("open passthrough flags = %v, %v", args, ok)
	}
}

func TestMergeDiscoveredModelChoicesNoOps(t *testing.T) {
	discovered := []DiscoveredModel{{ID: "brand-new", Label: "Brand New"}}

	t.Run("nothing discovered", func(t *testing.T) {
		schema := openModelSchema()
		if got := MergeDiscoveredModelChoices(schema, nil); !reflect.DeepEqual(got, schema) {
			t.Fatalf("schema changed with no discovered models: %+v", got)
		}
	})
	t.Run("no model option", func(t *testing.T) {
		schema := openModelSchema()[:1]
		if got := MergeDiscoveredModelChoices(schema, discovered); !reflect.DeepEqual(got, schema) {
			t.Fatalf("schema without a model option changed: %+v", got)
		}
	})
	t.Run("closed model option", func(t *testing.T) {
		// Without a FlagTemplate there is no flag shape to render a
		// discovered id with, so the option is left exactly as curated.
		schema := openModelSchema()
		schema[1].FlagTemplate = nil
		if got := MergeDiscoveredModelChoices(schema, discovered); !reflect.DeepEqual(got, schema) {
			t.Fatalf("closed model option changed: %+v", got)
		}
	})
	t.Run("everything already curated", func(t *testing.T) {
		schema := openModelSchema()
		if got := MergeDiscoveredModelChoices(schema, []DiscoveredModel{{ID: "opus"}, {ID: "claude-opus-5"}}); !reflect.DeepEqual(got, schema) {
			t.Fatalf("fully-curated listing changed the schema: %+v", got)
		}
	})
}

func TestResolveProviderMergesDiscoveredModels(t *testing.T) {
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M"})

	agent := &Agent{Name: "w", Provider: "cursor"}
	resolved, err := ResolveProvider(agent, &Workspace{}, BuiltinProviders(), fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	if len(fake.requests) != 1 {
		t.Fatalf("discoverer called %d times, want 1", len(fake.requests))
	}
	req := fake.requests[0]
	if req.Provider != "cursor" || req.Binary != "cursor-agent" || req.Fresh {
		t.Fatalf("request = %+v, want provider cursor, binary cursor-agent, not fresh", req)
	}
	if !reflect.DeepEqual(req.Discovery, ModelDiscovery{Args: []string{"--list-models"}, Format: "cursor-list-models"}) {
		t.Fatalf("request discovery = %+v", req.Discovery)
	}
	// The listing is account-scoped, so the harness credential env names
	// travel with the request for headless (env-key) logins.
	if !reflect.DeepEqual(req.PassthroughEnv, []string{"CURSOR_API_KEY"}) {
		t.Fatalf("PassthroughEnv = %v, want [CURSOR_API_KEY]", req.PassthroughEnv)
	}

	model := modelOptionOf(t, resolved.OptionsSchema)
	last := model.Choices[len(model.Choices)-1]
	want := OptionChoice{Value: "claude-opus-5-5-high", Label: "Claude Opus 5.5 1M", FlagArgs: []string{"--model", "claude-opus-5-5-high"}}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("last model choice = %+v, want %+v", last, want)
	}
	// The curated sentinel and seed are still in front, untouched.
	if model.Choices[0].Value != "" || model.Choices[1].Value != "auto" {
		t.Fatalf("curated seed reordered: %+v", model.Choices[:2])
	}
}

func TestResolveProviderSkipsDiscoveryWhenProviderDeclaresNone(t *testing.T) {
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "should-not-appear"})

	resolved, err := ResolveProvider(&Agent{Name: "w", Provider: "claude"}, &Workspace{}, BuiltinProviders(), fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("discoverer called for a provider with no listing verb: %+v", fake.requests)
	}
	if findChoice(modelOptionOf(t, resolved.OptionsSchema).Choices, "should-not-appear") != nil {
		t.Fatal("discovered id leaked into a curated-only provider")
	}
}

func TestResolveProviderWithoutDiscovererIsByteIdenticalToCurated(t *testing.T) {
	// No discoverer installed (the default, and every test process): the
	// resolved schema must equal the builtin one exactly.
	t.Cleanup(SetModelDiscoverer(nil))

	resolved, err := ResolveProvider(&Agent{Name: "w", Provider: "cursor"}, &Workspace{}, BuiltinProviders(), fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	want := modelOptionOf(t, BuiltinProviders()["cursor"].OptionsSchema)
	if got := modelOptionOf(t, resolved.OptionsSchema); !reflect.DeepEqual(got, want) {
		t.Fatalf("model option differs from the builtin catalog without a discoverer:\n%+v\nwant\n%+v", got, want)
	}
}

func TestResolveProviderDiscoveryFailureIsSoft(t *testing.T) {
	// A discoverer that finds nothing (binary absent, exec failed, output
	// unparseable) returns nil; resolution succeeds with the curated seed.
	fake := installFakeModelDiscoverer(t)

	resolved, err := ResolveProvider(&Agent{Name: "w", Provider: "grok"}, &Workspace{}, BuiltinProviders(), fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("discoverer called %d times, want 1", len(fake.requests))
	}
	want := modelOptionOf(t, BuiltinProviders()["grok"].OptionsSchema)
	if got := modelOptionOf(t, resolved.OptionsSchema); !reflect.DeepEqual(got, want) {
		t.Fatalf("failed discovery changed the curated model option:\n%+v\nwant\n%+v", got, want)
	}
}

func TestResolveProviderSkipsDiscoveryWhenWorkspaceStartCommandClearsSchema(t *testing.T) {
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "x"})

	ws := &Workspace{StartCommand: "cursor-agent --custom"}
	resolved, err := ResolveProvider(&Agent{Name: "w", Provider: "cursor"}, ws, BuiltinProviders(), fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if resolved.OptionsSchema != nil {
		t.Fatalf("start_command should clear OptionsSchema, got %+v", resolved.OptionsSchema)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("discoverer ran for a provider whose schema was cleared: %+v", fake.requests)
	}
}

func TestResolveProviderCustomChainInheritsModelDiscovery(t *testing.T) {
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "cursor-only-id", Label: "Cursor Only"})

	base := "builtin:cursor"
	providers := map[string]ProviderSpec{
		"fast": {Base: &base, DisplayName: "Fast cursor"},
	}
	resolved, err := ResolveProvider(&Agent{Name: "w", Provider: "fast"}, &Workspace{}, providers, fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(fake.requests) != 1 || fake.requests[0].Provider != "fast" || fake.requests[0].Binary != "cursor-agent" {
		t.Fatalf("requests = %+v, want one for provider fast / binary cursor-agent", fake.requests)
	}
	if resolved.ModelDiscovery == nil || resolved.ModelDiscovery.Format != "cursor-list-models" {
		t.Fatalf("ModelDiscovery did not propagate through the chain: %+v", resolved.ModelDiscovery)
	}
	if findChoice(modelOptionOf(t, resolved.OptionsSchema).Choices, "cursor-only-id") == nil {
		t.Fatal("discovered id missing from the chained provider's model choices")
	}
}

func TestModelDiscoveryDeclaredInCityTOML(t *testing.T) {
	// A custom provider declares its own listing verb in city.toml; the
	// declaration reaches the discoverer verbatim and the discovered ids
	// merge into the provider's open model option.
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "my/new-model", Label: "my/new-model"})

	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"

[providers.mycli]
command = "mycli"
prompt_mode = "arg"

[providers.mycli.model_discovery]
args = ["list-models", "--plain"]
format = "id-lines"
prefix = "my/"

[[providers.mycli.options_schema]]
key = "model"
label = "Model"
type = "select"
flag_template = ["--model", "{value}"]

  [[providers.mycli.options_schema.choices]]
  value = "my/curated"
  label = "Curated"
  flag_args = ["--model", "my/curated"]

[[agent]]
name = "worker"
provider = "mycli"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	spec := cfg.Providers["mycli"]
	wantDecl := &ModelDiscovery{Args: []string{"list-models", "--plain"}, Format: "id-lines", Prefix: "my/"}
	if !reflect.DeepEqual(spec.ModelDiscovery, wantDecl) {
		t.Fatalf("parsed model_discovery = %+v, want %+v", spec.ModelDiscovery, wantDecl)
	}

	resolved, err := ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, fakeLookPath)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if len(fake.requests) != 1 || !reflect.DeepEqual(fake.requests[0].Discovery, *wantDecl) || fake.requests[0].Binary != "mycli" {
		t.Fatalf("requests = %+v, want one carrying %+v for mycli", fake.requests, *wantDecl)
	}
	model := modelOptionOf(t, resolved.OptionsSchema)
	if findChoice(model.Choices, "my/curated") == nil || findChoice(model.Choices, "my/new-model") == nil {
		t.Fatalf("model choices = %+v, want curated + discovered", model.Choices)
	}

	// The eager cache carries the declaration too, deep-copied.
	cached, ok := ResolvedProviderCached(cfg, "mycli")
	if !ok {
		t.Fatal("mycli missing from the resolved provider cache")
	}
	if !reflect.DeepEqual(cached.ModelDiscovery, wantDecl) {
		t.Fatalf("cached ModelDiscovery = %+v, want %+v", cached.ModelDiscovery, wantDecl)
	}
	cached.ModelDiscovery.Args[0] = "mutated"
	again, _ := ResolvedProviderCached(cfg, "mycli")
	if again.ModelDiscovery.Args[0] == "mutated" {
		t.Fatal("ResolvedProviderCached aliases ModelDiscovery.Args with the cache entry")
	}
	if cfg.ResolvedProviders["mycli"].Provenance.FieldLayer["model_discovery"] != "providers.mycli" {
		t.Fatalf("model_discovery provenance = %q, want providers.mycli", cfg.ResolvedProviders["mycli"].Provenance.FieldLayer["model_discovery"])
	}
}

func TestDiscoverProviderOptionsForPicker(t *testing.T) {
	fake := installFakeModelDiscoverer(t, DiscoveredModel{ID: "grok-4.8", Label: "grok-4.8"})

	spec := BuiltinProviders()["grok"]
	schema := DiscoverProviderOptions(context.Background(), "grok", spec, true)
	if len(fake.requests) != 1 || !fake.requests[0].Fresh || fake.requests[0].Binary != "grok" {
		t.Fatalf("requests = %+v, want one fresh request for grok", fake.requests)
	}
	if !reflect.DeepEqual(fake.requests[0].PassthroughEnv, []string{"XAI_API_KEY"}) {
		t.Fatalf("PassthroughEnv = %v, want [XAI_API_KEY]", fake.requests[0].PassthroughEnv)
	}
	if findChoice(modelOptionOf(t, schema).Choices, "grok-4.8") == nil {
		t.Fatalf("picker schema lacks the discovered id: %+v", modelOptionOf(t, schema).Choices)
	}
	// The spec handed in is not mutated: the picker merge is per-read.
	if findChoice(modelOptionOf(t, spec.OptionsSchema).Choices, "grok-4.8") != nil {
		t.Fatal("DiscoverProviderOptions mutated the caller's spec")
	}

	// A provider with no declaration never reaches the discoverer.
	claude := BuiltinProviders()["claude"]
	if got := DiscoverProviderOptions(context.Background(), "claude", claude, true); !reflect.DeepEqual(got, claude.OptionsSchema) {
		t.Fatalf("claude schema changed: %+v", got)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("discoverer called for claude: %+v", fake.requests)
	}
}

func TestMergeProviderOverBuiltinCityModelDiscoveryWins(t *testing.T) {
	base := BuiltinProviders()["cursor"]
	city := ProviderSpec{ModelDiscovery: &ModelDiscovery{Args: []string{"list"}, Format: "id-lines"}}
	merged := MergeProviderOverBuiltin(base, city)
	if !reflect.DeepEqual(merged.ModelDiscovery, city.ModelDiscovery) {
		t.Fatalf("merged ModelDiscovery = %+v, want city's %+v", merged.ModelDiscovery, city.ModelDiscovery)
	}
	if merged.ModelDiscovery == city.ModelDiscovery {
		t.Fatal("merged ModelDiscovery aliases the city pointer")
	}
	inherit := MergeProviderOverBuiltin(base, ProviderSpec{DisplayName: "x"})
	if !reflect.DeepEqual(inherit.ModelDiscovery, base.ModelDiscovery) {
		t.Fatalf("nil city ModelDiscovery did not inherit the base: %+v", inherit.ModelDiscovery)
	}
}

func TestDeepCopiesCloneModelDiscovery(t *testing.T) {
	decl := &ModelDiscovery{Args: []string{"models"}, Format: "grok-models", Prefix: "p/"}

	spec := deepCopyProviderSpec(ProviderSpec{ModelDiscovery: decl})
	if !reflect.DeepEqual(spec.ModelDiscovery, decl) || spec.ModelDiscovery == decl || &spec.ModelDiscovery.Args[0] == &decl.Args[0] {
		t.Fatalf("deepCopyProviderSpec did not deep-copy ModelDiscovery: %+v", spec.ModelDiscovery)
	}

	rp := deepCopyResolvedProvider(ResolvedProvider{ModelDiscovery: decl})
	if !reflect.DeepEqual(rp.ModelDiscovery, decl) || rp.ModelDiscovery == decl || &rp.ModelDiscovery.Args[0] == &decl.Args[0] {
		t.Fatalf("deepCopyResolvedProvider did not deep-copy ModelDiscovery: %+v", rp.ModelDiscovery)
	}

	resolved := specToResolved("x", &ProviderSpec{ModelDiscovery: decl})
	if !reflect.DeepEqual(resolved.ModelDiscovery, decl) || resolved.ModelDiscovery == decl {
		t.Fatalf("specToResolved did not copy ModelDiscovery: %+v", resolved.ModelDiscovery)
	}

	folded := resolvedChainToSpec(ResolvedProvider{ModelDiscovery: decl}, ProviderSpec{})
	if !reflect.DeepEqual(folded.ModelDiscovery, decl) || folded.ModelDiscovery == decl {
		t.Fatalf("resolvedChainToSpec did not carry ModelDiscovery: %+v", folded.ModelDiscovery)
	}
	if nilCopy := deepCopyProviderSpec(ProviderSpec{}); nilCopy.ModelDiscovery != nil {
		t.Fatalf("nil ModelDiscovery became %+v", nilCopy.ModelDiscovery)
	}
}

func TestSetModelDiscovererRestore(t *testing.T) {
	first := &fakeModelDiscoverer{}
	restore := SetModelDiscoverer(first)
	if currentModelDiscoverer() != first {
		t.Fatal("SetModelDiscoverer did not install the discoverer")
	}
	restore()
	if currentModelDiscoverer() != nil {
		t.Fatal("restore did not put the previous (nil) discoverer back")
	}
}

// fakeLookPath resolves every binary so provider resolution never fails on
// PATH in these tests; the discoverer is what decides whether the binary is
// really launchable.
func fakeLookPath(name string) (string, error) { return "/usr/bin/" + name, nil }
