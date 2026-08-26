package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateWarnsOnUnhonoredModelPin(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "boomfartville",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "boomfartville"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/grok", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}
	agent := &config.Agent{
		Name:     "gorkcats",
		Provider: "grok",
		OptionDefaults: map[string]string{
			"model":           "not-a-grok-model",
			"effort":          "high",
			"permission_mode": "unrestricted",
		},
	}

	tp, err := resolveTemplate(params, agent, "gascity/gasburger.gorkcats", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if strings.Contains(tp.Command, "not-a-grok-model") {
		t.Fatalf("command honored unknown model: %q", tp.Command)
	}
	if !strings.Contains(tp.Command, "--effort high") {
		t.Fatalf("command dropped valid effort pin: %q", tp.Command)
	}
	got := stderr.String()
	for _, want := range []string{
		"WARNING:",
		`agent "gascity/gasburger.gorkcats"`,
		`model="not-a-grok-model"`,
		"flag omitted",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr %q missing %q", got, want)
		}
	}
}

func TestResolveTemplateHonorsGrok46Pin(t *testing.T) {
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	params := &agentBuildParams{
		fs:              fsys.OSFS{},
		cityName:        "boomfartville",
		cityPath:        cityPath,
		workspace:       &config.Workspace{Name: "boomfartville"},
		providers:       config.BuiltinProviders(),
		lookPath:        func(string) (string, error) { return "/usr/bin/grok", nil },
		beaconTime:      testBeaconTime,
		sessionTemplate: "",
		beadNames:       make(map[string]string),
		stderr:          &stderr,
	}
	agent := &config.Agent{
		Name:     "gorkcats",
		Provider: "grok",
		OptionDefaults: map[string]string{
			"model":           "grok-4.6",
			"effort":          "high",
			"permission_mode": "unrestricted",
		},
	}

	tp, err := resolveTemplate(params, agent, "gascity/gasburger.gorkcats", nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if !strings.Contains(tp.Command, "--model grok-4.6") {
		t.Fatalf("command missing grok-4.6 model flag: %q", tp.Command)
	}
	if !strings.Contains(tp.Command, "--effort high") {
		t.Fatalf("command missing effort flag: %q", tp.Command)
	}
	if strings.Contains(stderr.String(), "WARNING:") {
		t.Fatalf("valid grok-4.6 pin warned: %q", stderr.String())
	}
}
