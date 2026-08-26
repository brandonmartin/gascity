package builtin

import (
	"bufio"
	"os/exec"
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

// TestBuiltinCursorCatalogMatchesBinary is the drift guard: the catalog is a
// generated snapshot of `cursor-agent --list-models`, so any id the binary
// offers and the catalog lacks means the snapshot is stale. Skipped where
// cursor-agent is not installed (CI); regenerate with
// scripts/gen-cursor-models.sh.
//
// Extra catalog ids are reported but not fatal: `--list-models` is
// account-scoped, so a narrower account legitimately sees fewer.
func TestBuiltinCursorCatalogMatchesBinary(t *testing.T) {
	bin, err := exec.LookPath("cursor-agent")
	if err != nil {
		t.Skip("cursor-agent not installed; catalog drift not checkable here")
	}
	out, err := exec.Command(bin, "--list-models").Output()
	if err != nil {
		t.Skipf("cursor-agent --list-models failed (not logged in?): %v", err)
	}

	live := parseCursorModelList(string(out))
	if len(live) == 0 {
		t.Skip("cursor-agent --list-models returned no ids")
	}

	have := make(map[string]string, len(cursorModels))
	for _, model := range cursorModels {
		have[model.ID] = model.Label
	}
	for _, model := range live {
		label, ok := have[model.ID]
		if !ok {
			t.Errorf("cursor-agent offers %q (%s), catalog does not — run scripts/gen-cursor-models.sh", model.ID, model.Label)
			continue
		}
		if label != model.Label {
			t.Errorf("%s label = %q, cursor-agent says %q — run scripts/gen-cursor-models.sh", model.ID, label, model.Label)
		}
		delete(have, model.ID)
	}
	for id := range have {
		t.Logf("catalog lists %q, this account's cursor-agent does not (account scoping or a removed id)", id)
	}
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
