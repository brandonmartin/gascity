package builtin

// cursorModel is one entry of the cursor-agent model catalog: the id accepted
// by `cursor-agent --model <id>` and the display name cursor-agent prints for
// it in `--list-models`.
//
// The catalog itself lives in cursor_models.go, which is generated from the
// binary by scripts/gen-cursor-models.sh rather than hand-transcribed — 200+
// ids maintained by hand is how the previous catalog drifted to zero (ga-cot).
type cursorModel struct {
	ID    string
	Label string
}

// cursorModelOption builds the cursor "model" schema option from the generated
// catalog.
//
// Cursor folds the effort axis INTO the model id (claude-opus-5-low ...
// claude-opus-5-xhigh, plus an orthogonal "-fast" variant), so there is no
// separate effort option for this provider: `cursor-agent --help` publishes no
// --effort flag, and emitting one would be rejected by the CLI. Pinning effort
// on cursor means pinning the composed id.
//
// The option has no default: with --model absent cursor-agent selects "auto"
// itself, and a pin stays an explicit operator choice.
func cursorModelOption() BuiltinProviderOption {
	choices := make([]BuiltinOptionChoice, 0, len(cursorModels)+1)
	choices = append(choices, BuiltinOptionChoice{Value: "", Label: "Default"})
	for _, model := range cursorModels {
		choices = append(choices, BuiltinOptionChoice{
			Value: model.ID,
			Label: model.Label,
			// cursor-agent publishes no -m short form, so no FlagAliases.
			FlagArgs: []string{"--model", model.ID},
		})
	}
	return BuiltinProviderOption{
		Key:     "model",
		Label:   "Model",
		Type:    "select",
		Choices: choices,
		// Open option (ga-fyh): the generated catalog is a suggestion list, not a
		// closed enum. cursor's catalog is account-scoped and includes
		// parameterized bracket ids such as
		// claude-opus-4-8[context=1m,effort=high], so any id must reach the CLI.
		FlagTemplate: []string{"--model", optionValuePlaceholder},
	}
}
