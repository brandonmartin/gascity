package builtin

import (
	"slices"
	"testing"
)

// TestBuiltinGrokModelChoicesIncludeGrok46And47 is the grok half of ga-fyh:
// gasburger.refinery and gasburger.gorkcats pin model = "grok-4.6", and the
// enum must also accept grok-4.7 ahead of the next rollout. A value missing
// from this closed enum yields no FlagArgs, so the launch path silently
// omits --model (same hole as ra-jbbv0 for claude-opus-5).
func TestBuiltinGrokModelChoicesIncludeGrok46And47(t *testing.T) {
	grok, ok := BuiltinProviders()["grok"]
	if !ok {
		t.Fatal("BuiltinProviders() missing grok")
	}
	for _, value := range []string{"grok-4.6", "grok-4.7"} {
		args := mustChoiceFlagArgs(t, grok, "model", value)
		want := []string{"--model", value}
		if !slices.Equal(args, want) {
			t.Errorf("%s FlagArgs = %v, want %v", value, args, want)
		}
	}
}

// TestGasburgerPackPinsResolveToFlagArgs asserts that every model and effort
// pin used by the shipped gasburger pack resolves to real FlagArgs for its
// provider. This would have caught grok-4.6 on day one.
func TestGasburgerPackPinsResolveToFlagArgs(t *testing.T) {
	providers := BuiltinProviders()
	pins := []struct {
		agent    string
		provider string
		key      string
		value    string
	}{
		{agent: "refinery", provider: "grok", key: "model", value: "grok-4.6"},
		{agent: "refinery", provider: "grok", key: "effort", value: "high"},
		{agent: "gorkcats", provider: "grok", key: "model", value: "grok-4.6"},
		{agent: "gorkcats", provider: "grok", key: "effort", value: "high"},
		{agent: "mayor", provider: "claude", key: "model", value: "opus-5"},
		{agent: "mayor", provider: "claude", key: "effort", value: "medium"},
		{agent: "clodcats", provider: "claude", key: "model", value: "opus-5"},
		{agent: "clodcats", provider: "claude", key: "effort", value: "medium"},
		{agent: "fabcats", provider: "claude", key: "model", value: "fable-5"},
		{agent: "fabcats", provider: "claude", key: "effort", value: "high"},
		{agent: "crew", provider: "claude", key: "model", value: "fable-5"},
		{agent: "crew", provider: "claude", key: "effort", value: "medium"},
		{agent: "kittens", provider: "claude", key: "model", value: "sonnet"},
		{agent: "kittens", provider: "claude", key: "effort", value: "medium"},
		{agent: "boot", provider: "claude", key: "model", value: "sonnet"},
		{agent: "boot", provider: "claude", key: "effort", value: "low"},
		{agent: "deacon", provider: "claude", key: "model", value: "sonnet"},
		{agent: "deacon", provider: "claude", key: "effort", value: "medium"},
		{agent: "dog", provider: "claude", key: "model", value: "sonnet"},
		{agent: "dog", provider: "claude", key: "effort", value: "medium"},
		{agent: "worker", provider: "claude", key: "model", value: "sonnet"},
		{agent: "worker", provider: "claude", key: "effort", value: "medium"},
		{agent: "witness", provider: "claude", key: "model", value: "sonnet"},
		{agent: "witness", provider: "claude", key: "effort", value: "medium"},
	}
	for _, pin := range pins {
		spec, ok := providers[pin.provider]
		if !ok {
			t.Errorf("%s: BuiltinProviders() missing %q", pin.agent, pin.provider)
			continue
		}
		args := mustChoiceFlagArgs(t, spec, pin.key, pin.value)
		if len(args) == 0 {
			t.Errorf("%s: provider %s %s=%q has empty FlagArgs (launch would omit the flag)",
				pin.agent, pin.provider, pin.key, pin.value)
		}
	}
}

func mustChoiceFlagArgs(t *testing.T, spec BuiltinProviderSpec, key, value string) []string {
	t.Helper()
	for _, opt := range spec.OptionsSchema {
		if opt.Key != key {
			continue
		}
		for _, choice := range opt.Choices {
			if choice.Value == value {
				return choice.FlagArgs
			}
		}
		t.Fatalf("provider %q option %q missing choice %q", spec.DisplayName, key, value)
	}
	t.Fatalf("provider %q missing option %q", spec.DisplayName, key)
	return nil
}
