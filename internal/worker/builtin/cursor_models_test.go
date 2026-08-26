package builtin

import (
	"bufio"
	"sort"
	"strings"
	"testing"
)

// cursorModelOptionFor returns the cursor provider's model option.
func cursorModelOptionFor(t *testing.T) BuiltinProviderOption {
	t.Helper()
	spec, ok := BuiltinProviders()["cursor"]
	if !ok {
		t.Fatal("BuiltinProviders() missing cursor")
	}
	for _, option := range spec.OptionsSchema {
		if option.Key == "model" {
			return option
		}
	}
	t.Fatal("cursor provider missing model option")
	return BuiltinProviderOption{}
}

func TestBuiltinCursorModelChoicesCoverCatalog(t *testing.T) {
	spec := BuiltinProviders()["cursor"]
	modelOption := cursorModelOptionFor(t)

	// cursor-agent picks its own model ("auto") when --model is absent, and a
	// pin must stay an explicit operator choice, so the option never fires by
	// default.
	if modelOption.Default != "" {
		t.Errorf("model Default = %q, want empty", modelOption.Default)
	}
	if value, ok := spec.OptionDefaults["model"]; ok {
		t.Errorf("OptionDefaults[model] = %q, want absent", value)
	}

	if len(modelOption.Choices) == 0 || modelOption.Choices[0].Value != "" || len(modelOption.Choices[0].FlagArgs) != 0 {
		t.Fatalf("model choices must lead with the empty no-flag sentinel, got %v", modelOption.Choices)
	}
	if len(modelOption.Choices) != len(cursorModels)+1 {
		t.Fatalf("model choice count = %d, want %d (sentinel + %d catalog ids)",
			len(modelOption.Choices), len(cursorModels)+1, len(cursorModels))
	}

	seen := make(map[string]bool, len(modelOption.Choices))
	for _, choice := range modelOption.Choices[1:] {
		if seen[choice.Value] {
			t.Errorf("duplicate model choice %q", choice.Value)
		}
		seen[choice.Value] = true
		if choice.Label == "" {
			t.Errorf("model choice %q has no label", choice.Value)
		}
		if len(choice.FlagArgs) != 2 || choice.FlagArgs[0] != "--model" || choice.FlagArgs[1] != choice.Value {
			t.Errorf("%s FlagArgs = %v, want [--model %s]", choice.Value, choice.FlagArgs, choice.Value)
		}
		// `cursor-agent --help` publishes no -m short form.
		if len(choice.FlagAliases) != 0 {
			t.Errorf("%s FlagAliases = %v, want none (cursor-agent defines no -m)", choice.Value, choice.FlagAliases)
		}
	}

	// The catalog is worthless if it lost the families the audit named: every
	// effort tier of the frontier models, and the cursor-only grok ids.
	for _, want := range []string{
		"auto",
		"claude-opus-5-high",
		"claude-opus-5-thinking-xhigh",
		"claude-sonnet-5-high",
		"claude-fable-5-high",
		"cursor-grok-4.6-high",
		"gpt-5.3-codex-xhigh",
		"gemini-3.7-flash-high",
	} {
		if !seen[want] {
			t.Errorf("model choices missing %q", want)
		}
	}
}

// TestBuiltinCursorExpressesEffortInModelID pins the decision recorded in
// ga-cot: cursor-agent has no --effort flag. Effort is a suffix of the model id
// (claude-opus-5-low ... -xhigh), so declaring a separate effort key here would
// emit a flag the CLI rejects.
func TestBuiltinCursorExpressesEffortInModelID(t *testing.T) {
	spec := BuiltinProviders()["cursor"]
	for _, option := range spec.OptionsSchema {
		if option.Key == "effort" {
			t.Fatalf("cursor declares an effort option (%v); cursor-agent has no --effort flag — effort is a model-id suffix", option)
		}
	}

	tiers := map[string]bool{}
	for _, model := range cursorModels {
		for _, tier := range []string{"low", "medium", "high", "xhigh", "max"} {
			if strings.HasSuffix(model.ID, "-"+tier) {
				tiers[tier] = true
			}
		}
	}
	for _, tier := range []string{"low", "high", "xhigh"} {
		if !tiers[tier] {
			t.Errorf("catalog reaches no %q-effort model id", tier)
		}
	}
}

// TestParseCursorModelList pins the `cursor-agent --list-models` line format
// the generator (scripts/gen-cursor-models.sh) and catalog snapshot share.
// Header, tip, and malformed lines are skipped; body lines are "<id> - <label>".
func TestParseCursorModelList(t *testing.T) {
	const listing = "" +
		"Available models:\n" +
		"\n" +
		"auto - Auto (default)\n" +
		"claude-opus-5-high - Claude Opus 5 High\n" +
		"id with space - not an id\n" +
		"no-separator-here\n" +
		" - missing id\n" +
		"missing-label - \n" +
		"cursor-grok-4.6-high - Cursor Grok 4.6 High\n" +
		"Tip: pin with --model <id>\n"

	got := parseCursorModelList(listing)
	want := []cursorModel{
		{ID: "auto", Label: "Auto (default)"},
		{ID: "claude-opus-5-high", Label: "Claude Opus 5 High"},
		{ID: "cursor-grok-4.6-high", Label: "Cursor Grok 4.6 High"},
	}
	if len(got) != len(want) {
		t.Fatalf("parseCursorModelList got %d models, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBuiltinCursorCatalogMatchesListing is the drift guard without a live
// subprocess: the catalog is a generated snapshot of `cursor-agent --list-models`.
// Any id the listing offers and the catalog lacks means the snapshot is stale;
// regenerate with scripts/gen-cursor-models.sh. Extra catalog ids are reported
// but not fatal — `--list-models` is account-scoped, so a narrower account
// legitimately sees fewer.
func TestBuiltinCursorCatalogMatchesListing(t *testing.T) {
	catalog := []cursorModel{
		{ID: "auto", Label: "Auto (default)"},
		{ID: "claude-opus-5-high", Label: "Claude Opus 5 High"},
		{ID: "account-only", Label: "Only this snapshot"},
	}

	t.Run("listing ids must be in the catalog", func(t *testing.T) {
		live := parseCursorModelList("" +
			"Available models:\n" +
			"auto - Auto (default)\n" +
			"claude-opus-5-high - Claude Opus 5 High\n" +
			"brand-new-id - Brand New\n")
		diff := diffCursorCatalog(catalog, live)
		if len(diff.missing) != 1 || diff.missing[0].ID != "brand-new-id" {
			t.Fatalf("missing = %v, want [brand-new-id]", diff.missing)
		}
		if len(diff.labelDrift) != 0 {
			t.Fatalf("labelDrift = %v, want none", diff.labelDrift)
		}
	})

	t.Run("label drift is reported", func(t *testing.T) {
		live := parseCursorModelList("auto - Auto renamed\n")
		diff := diffCursorCatalog(catalog, live)
		if len(diff.labelDrift) != 1 || diff.labelDrift[0].ID != "auto" || diff.labelDrift[0].Label != "Auto renamed" {
			t.Fatalf("labelDrift = %v, want auto/Auto renamed", diff.labelDrift)
		}
	})

	t.Run("extra catalog ids are non-fatal", func(t *testing.T) {
		live := parseCursorModelList("auto - Auto (default)\nclaude-opus-5-high - Claude Opus 5 High\n")
		diff := diffCursorCatalog(catalog, live)
		if len(diff.missing) != 0 || len(diff.labelDrift) != 0 {
			t.Fatalf("unexpected fatal drift: missing=%v labelDrift=%v", diff.missing, diff.labelDrift)
		}
		if len(diff.extraCatalog) != 1 || diff.extraCatalog[0] != "account-only" {
			t.Fatalf("extraCatalog = %v, want [account-only]", diff.extraCatalog)
		}
	})

	t.Run("generated catalog matches a listing of itself", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("Available models:\n\n")
		for _, model := range cursorModels {
			b.WriteString(model.ID)
			b.WriteString(" - ")
			b.WriteString(model.Label)
			b.WriteByte('\n')
		}
		diff := diffCursorCatalog(cursorModels, parseCursorModelList(b.String()))
		if len(diff.missing) != 0 || len(diff.labelDrift) != 0 || len(diff.extraCatalog) != 0 {
			t.Fatalf("self-listing drifted: missing=%v labelDrift=%v extra=%v",
				diff.missing, diff.labelDrift, diff.extraCatalog)
		}
	})
}

type cursorCatalogDiff struct {
	missing      []cursorModel
	labelDrift   []cursorModel
	extraCatalog []string
}

// diffCursorCatalog is the function seam the drift tests drive: compare a
// catalog snapshot against a parsed --list-models listing. Live cursor-agent
// invocation belongs to scripts/gen-cursor-models.sh, not the unit tests.
func diffCursorCatalog(catalog, live []cursorModel) cursorCatalogDiff {
	have := make(map[string]string, len(catalog))
	for _, model := range catalog {
		have[model.ID] = model.Label
	}
	var diff cursorCatalogDiff
	for _, model := range live {
		label, ok := have[model.ID]
		if !ok {
			diff.missing = append(diff.missing, model)
			continue
		}
		if label != model.Label {
			diff.labelDrift = append(diff.labelDrift, model)
		}
		delete(have, model.ID)
	}
	for id := range have {
		diff.extraCatalog = append(diff.extraCatalog, id)
	}
	sort.Strings(diff.extraCatalog)
	return diff
}

// parseCursorModelList parses `cursor-agent --list-models` output, whose body
// lines are "<id> - <label>".
func parseCursorModelList(out string) []cursorModel {
	var models []cursorModel
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		id, label, ok := strings.Cut(strings.TrimSpace(scanner.Text()), " - ")
		if !ok || id == "" || label == "" || strings.ContainsAny(id, " \t") {
			continue
		}
		models = append(models, cursorModel{ID: id, Label: label})
	}
	return models
}
