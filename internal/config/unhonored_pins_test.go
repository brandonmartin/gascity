package config

import (
	"slices"
	"strings"
	"testing"
)

func TestBuiltinProviderOptionDefaultsAreHonored(t *testing.T) {
	for name, spec := range BuiltinProviders() {
		rp := &ResolvedProvider{
			Name:              name,
			OptionsSchema:     spec.OptionsSchema,
			EffectiveDefaults: ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, nil),
		}
		if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
			t.Errorf("provider %q builtin defaults are unhonored: %+v", name, pins)
		}
	}
}

func TestGrok46AgentPinEmitsModelFlag(t *testing.T) {
	grok := BuiltinProviders()["grok"]
	rp := &ResolvedProvider{
		Name:          "grok",
		OptionsSchema: grok.OptionsSchema,
		EffectiveDefaults: ComputeEffectiveDefaults(grok.OptionsSchema, grok.OptionDefaults, map[string]string{
			"model":  "grok-4.6",
			"effort": "high",
		}),
	}
	if pins := rp.UnhonoredOptionPins(); len(pins) > 0 {
		t.Fatalf("gasburger grok-4.6 pin unhonored: %+v", pins)
	}
	args := rp.ResolveDefaultArgs()
	if !containsFlagValue(args, "--model", "grok-4.6") {
		t.Errorf("ResolveDefaultArgs() = %v, want --model grok-4.6", args)
	}
	if !containsFlagValue(args, "--effort", "high") {
		t.Errorf("ResolveDefaultArgs() = %v, want --effort high", args)
	}
}

func TestUnhonoredOptionPinsUnknownValue(t *testing.T) {
	grok := BuiltinProviders()["grok"]
	rp := &ResolvedProvider{
		Name:              "grok",
		OptionsSchema:     grok.OptionsSchema,
		EffectiveDefaults: map[string]string{"model": "not-a-grok-model"},
	}
	pins := rp.UnhonoredOptionPins()
	if len(pins) != 1 {
		t.Fatalf("UnhonoredOptionPins() = %+v, want one unknown_value pin", pins)
	}
	if pins[0].Key != "model" || pins[0].Value != "not-a-grok-model" || pins[0].Reason != UnhonoredPinUnknownValue {
		t.Errorf("pin = %+v, want model=not-a-grok-model unknown_value", pins[0])
	}
	if !slices.Contains(pins[0].Valid, "grok-4.6") {
		t.Errorf("valid set %v missing grok-4.6", pins[0].Valid)
	}
	args := rp.ResolveDefaultArgs()
	if containsFlagValue(args, "--model", "not-a-grok-model") {
		t.Errorf("ResolveDefaultArgs() emitted unknown model: %v", args)
	}
	warn := FormatUnhonoredOptionPin("gascity/gasburger.gorkcats", "grok", pins[0])
	for _, want := range []string{
		"WARNING:",
		`agent "gascity/gasburger.gorkcats"`,
		`model="not-a-grok-model"`,
		`provider "grok"`,
		"grok-4.6",
		"flag omitted",
	} {
		if !strings.Contains(warn, want) {
			t.Errorf("warning %q missing %q", warn, want)
		}
	}
}

func TestUnhonoredOptionPinsUnknownKey(t *testing.T) {
	gemini := BuiltinProviders()["gemini"]
	rp := &ResolvedProvider{
		Name:          "gemini",
		OptionsSchema: gemini.OptionsSchema,
		EffectiveDefaults: map[string]string{
			"permission_mode": "unrestricted",
			"effort":          "high",
		},
	}
	pins := rp.UnhonoredOptionPins()
	if len(pins) != 1 || pins[0].Key != "effort" || pins[0].Reason != UnhonoredPinUnknownOption {
		t.Fatalf("UnhonoredOptionPins() = %+v, want effort unknown_option", pins)
	}
	warn := FormatUnhonoredOptionPin("reviewer", "gemini", pins[0])
	if !strings.Contains(warn, `effort="high"`) || !strings.Contains(warn, "not in the") {
		t.Errorf("warning %q", warn)
	}
}

func TestGasburgerPackPinsResolveDefaultArgs(t *testing.T) {
	type pin struct {
		provider string
		key      string
		value    string
	}
	pins := []pin{
		{provider: "grok", key: "model", value: "grok-4.6"},
		{provider: "grok", key: "effort", value: "high"},
		{provider: "claude", key: "model", value: "opus-5"},
		{provider: "claude", key: "effort", value: "medium"},
		{provider: "claude", key: "model", value: "fable-5"},
		{provider: "claude", key: "effort", value: "high"},
		{provider: "claude", key: "model", value: "sonnet"},
		{provider: "claude", key: "effort", value: "low"},
	}
	builtins := BuiltinProviders()
	for _, p := range pins {
		spec, ok := builtins[p.provider]
		if !ok {
			t.Fatalf("missing provider %q", p.provider)
		}
		rp := &ResolvedProvider{
			Name:          p.provider,
			OptionsSchema: spec.OptionsSchema,
			EffectiveDefaults: ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, map[string]string{
				p.key: p.value,
			}),
		}
		if unhonored := rp.UnhonoredOptionPins(); len(unhonored) > 0 {
			t.Errorf("%s %s=%q unhonored: %+v", p.provider, p.key, p.value, unhonored)
			continue
		}
		args := rp.ResolveDefaultArgs()
		if len(args) == 0 {
			t.Errorf("%s %s=%q produced no FlagArgs", p.provider, p.key, p.value)
		}
	}
}

func containsFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}
