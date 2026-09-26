package builtin

import (
	"errors"
	"reflect"
	"testing"
)

// grokModelsListing is `grok models` as printed by grok 1.0.41 (4220f3b224a6)
// on 2026-09-25. The banner and "Default model:" lines are not bullet entries
// and must be skipped; the default marker suffix is not part of the id.
const grokModelsListing = "" +
	"You are logged in with grok.com.\n" +
	"\n" +
	"Default model: grok-4.7\n" +
	"\n" +
	"Available models:\n" +
	"  * grok-4.7 (default)\n" +
	"  - grok-4.7-build-fast\n" +
	"  - grok-4.6\n" +
	"  - grok-4.5\n"

// cursorListModelsListing is the head of `cursor-agent --list-models` as
// printed by cursor-agent 2026.09.15-d2fe57e, plus the trailing tip line the
// older format printed. Only "<id> - <label>" body lines are entries.
const cursorListModelsListing = "" +
	"Available models\n" +
	"\n" +
	"auto - Auto (default)\n" +
	"gpt-5.3-codex-low - Codex 5.3 Low\n" +
	"claude-fable-5-1-thinking-xhigh - Claude Fable 5.1 1M Extra High Thinking (NO ZDR)\n" +
	"claude-opus-4-8[context=1m,effort=high] - Claude Opus 4.8 1M High\n" +
	"Tip: pin with --model <id>\n"

func TestParseModelListingCursor(t *testing.T) {
	got, err := ParseModelListing(ModelListFormatCursor, cursorListModelsListing)
	if err != nil {
		t.Fatalf("ParseModelListing: %v", err)
	}
	want := []DiscoveredModel{
		{ID: "auto", Label: "Auto (default)"},
		{ID: "gpt-5.3-codex-low", Label: "Codex 5.3 Low"},
		{ID: "claude-fable-5-1-thinking-xhigh", Label: "Claude Fable 5.1 1M Extra High Thinking (NO ZDR)"},
		{ID: "claude-opus-4-8[context=1m,effort=high]", Label: "Claude Opus 4.8 1M High"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v\nwant %+v", got, want)
	}
}

func TestParseModelListingGrok(t *testing.T) {
	got, err := ParseModelListing(ModelListFormatGrok, grokModelsListing)
	if err != nil {
		t.Fatalf("ParseModelListing: %v", err)
	}
	want := []DiscoveredModel{
		{ID: "grok-4.7", Label: "grok-4.7"},
		{ID: "grok-4.7-build-fast", Label: "grok-4.7-build-fast"},
		{ID: "grok-4.6", Label: "grok-4.6"},
		{ID: "grok-4.5", Label: "grok-4.5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v\nwant %+v", got, want)
	}
}

func TestParseModelListingIDLines(t *testing.T) {
	// opencode-style: one "provider/model" id per line. Anything that is not
	// a bare id token (banners, headers, prose, blank lines) is dropped, and
	// repeated ids collapse to their first occurrence.
	listing := "" +
		"opencode v1.2.3\n" +
		"\n" +
		"Available models:\n" +
		"cerebras/gpt-oss-120b\n" +
		"  cerebras/zai-glm-4.7  \n" +
		"groq/openai/gpt-oss-120b\n" +
		"cerebras/gpt-oss-120b\n" +
		"not an id because of spaces\n" +
		"warning: something happened\n"
	got, err := ParseModelListing(ModelListFormatIDLines, listing)
	if err != nil {
		t.Fatalf("ParseModelListing: %v", err)
	}
	want := []DiscoveredModel{
		{ID: "cerebras/gpt-oss-120b", Label: "cerebras/gpt-oss-120b"},
		{ID: "cerebras/zai-glm-4.7", Label: "cerebras/zai-glm-4.7"},
		{ID: "groq/openai/gpt-oss-120b", Label: "groq/openai/gpt-oss-120b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %+v\nwant %+v", got, want)
	}
}

func TestParseModelListingRejectsUnknownFormat(t *testing.T) {
	if _, err := ParseModelListing("no-such-format", "auto - Auto\n"); !errors.Is(err, ErrUnknownModelListFormat) {
		t.Fatalf("err = %v, want ErrUnknownModelListFormat", err)
	}
}

func TestParseModelListingNoEntriesIsAnError(t *testing.T) {
	// A listing that parses to nothing is indistinguishable from a wrong
	// verb, a logged-out CLI, or a changed output format. Callers must treat
	// it as a failed discovery (keep the curated seed), never as "this
	// provider has no models".
	for _, format := range ModelListFormats() {
		if _, err := ParseModelListing(format, "Error: Authentication required.\n"); !errors.Is(err, ErrNoModelsParsed) {
			t.Errorf("%s: err = %v, want ErrNoModelsParsed", format, err)
		}
	}
}

func TestParseModelListingDedupesIDs(t *testing.T) {
	got, err := ParseModelListing(ModelListFormatCursor, "auto - Auto\nauto - Auto renamed\n")
	if err != nil {
		t.Fatalf("ParseModelListing: %v", err)
	}
	if len(got) != 1 || got[0].Label != "Auto" {
		t.Fatalf("models = %+v, want the first auto entry only", got)
	}
}

// TestBuiltinModelDiscoveryDeclarationsAreWellFormed guards every seed
// declaration: the listing verb must be non-empty, the format must name a
// parser, and the provider must expose an OPEN "model" option — discovered ids
// are rendered through that option's FlagTemplate, so a closed option would
// have nothing to render them with.
func TestBuiltinModelDiscoveryDeclarationsAreWellFormed(t *testing.T) {
	formats := map[string]bool{}
	for _, f := range ModelListFormats() {
		formats[f] = true
	}
	declared := 0
	for name, spec := range BuiltinProviders() {
		if spec.ModelDiscovery == nil {
			continue
		}
		declared++
		if len(spec.ModelDiscovery.Args) == 0 {
			t.Errorf("%s: ModelDiscovery.Args is empty", name)
		}
		if !formats[spec.ModelDiscovery.Format] {
			t.Errorf("%s: ModelDiscovery.Format %q is not a known listing format", name, spec.ModelDiscovery.Format)
		}
		var model *BuiltinProviderOption
		for i := range spec.OptionsSchema {
			if spec.OptionsSchema[i].Key == "model" {
				model = &spec.OptionsSchema[i]
			}
		}
		if model == nil {
			t.Errorf("%s: declares model discovery but has no model option", name)
			continue
		}
		if len(model.FlagTemplate) == 0 {
			t.Errorf("%s: model option is closed (no FlagTemplate); discovered ids could not be rendered", name)
		}
	}
	if declared == 0 {
		t.Fatal("no builtin provider declares model discovery")
	}
}

// TestBuiltinModelDiscoverySeeds pins the per-provider findings recorded on
// ga-k67: which CLIs publish a listing verb and which do not.
func TestBuiltinModelDiscoverySeeds(t *testing.T) {
	specs := BuiltinProviders()

	want := map[string]BuiltinModelDiscovery{
		// Verified: cursor-agent 2026.09.15 prints "<id> - <label>" lines.
		"cursor": {Args: []string{"--list-models"}, Format: ModelListFormatCursor},
		// Verified: grok 1.0.41 `grok models` prints bullet lines.
		"grok": {Args: []string{"models"}, Format: ModelListFormatGrok},
		// opencode publishes `opencode models`, one provider/model id per line.
		"opencode": {Args: []string{"models"}, Format: ModelListFormatIDLines},
		"cerebras": {Args: []string{"models"}, Format: ModelListFormatIDLines, Prefix: "cerebras/"},
		"groq":     {Args: []string{"models"}, Format: ModelListFormatIDLines, Prefix: "groq/"},
		"mimocode": {Args: []string{"models"}, Format: ModelListFormatIDLines},
	}
	for name, decl := range want {
		got := specs[name].ModelDiscovery
		if got == nil {
			t.Errorf("%s: ModelDiscovery = nil, want %+v", name, decl)
			continue
		}
		if !reflect.DeepEqual(*got, decl) {
			t.Errorf("%s: ModelDiscovery = %+v, want %+v", name, *got, decl)
		}
	}

	// CLIs with no listing verb stay curated-only. Verified 2026-09-25:
	// `codex --help` publishes no models subcommand or --list-models flag
	// (codex-cli 0.157.0); `claude --help` publishes none (2.1.282).
	for _, name := range []string{"claude", "codex", "gemini", "kimi", "kiro", "copilot", "amp", "zcode", "auggie", "pi", "omp", "antigravity"} {
		if specs[name].ModelDiscovery != nil {
			t.Errorf("%s: ModelDiscovery = %+v, want nil (no listing verb)", name, *specs[name].ModelDiscovery)
		}
	}
}

func TestBuiltinProvidersDeepCopiesModelDiscovery(t *testing.T) {
	first := BuiltinProviders()["cursor"]
	second := BuiltinProviders()["cursor"]
	if first.ModelDiscovery == nil || second.ModelDiscovery == nil {
		t.Fatal("cursor must declare model discovery")
	}
	if first.ModelDiscovery == second.ModelDiscovery {
		t.Fatal("ModelDiscovery pointer is shared between BuiltinProviders() calls")
	}
	first.ModelDiscovery.Args[0] = "mutated"
	if second.ModelDiscovery.Args[0] == "mutated" {
		t.Fatal("ModelDiscovery.Args backing array is shared between BuiltinProviders() calls")
	}
}
