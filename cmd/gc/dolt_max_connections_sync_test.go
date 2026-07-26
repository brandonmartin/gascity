package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestDoltMaxConnectionsFallbacksMatchManagedDefault keeps the shell-side
// copies of the managed connection cap equal to config.DefaultDoltMaxConnections.
//
// gc only exports GC_DOLT_MAX_CONNECTIONS when city.toml sets [dolt]
// max_connections explicitly, so the literals in these scripts are the managed
// default on every path that does not go through the Go config writer. Letting
// them drift silently reinstates the old cap: gc-beads-bd.sh writes the cap the
// server actually binds when the shell path generates dolt-config.yaml, and
// mol-dog-doctor.sh sizes the "near capacity" advisory against it whenever the
// live @@GLOBAL.max_connections query fails. The cap that exhausted during the
// ga-oigp outage took the whole data plane down, so a silent revert is not a
// cosmetic drift.
func TestDoltMaxConnectionsFallbacksMatchManagedDefault(t *testing.T) {
	root := repoRootForLint(t)
	want := strconv.Itoa(config.DefaultDoltMaxConnections)

	tests := []struct {
		name    string
		path    []string
		pattern *regexp.Regexp
	}{
		{
			name:    "gc-beads-bd.sh config writer fallback",
			path:    []string{"examples", "bd", "assets", "scripts", "gc-beads-bd.sh"},
			pattern: regexp.MustCompile(`max_connections=(?:\$\{GC_DOLT_MAX_CONNECTIONS:-)?(\d+)`),
		},
		{
			name:    "mol-dog-doctor.sh advisory fallback",
			path:    []string{"examples", "bd", "dolt", "assets", "scripts", "mol-dog-doctor.sh"},
			pattern: regexp.MustCompile(`CONN_MAX=(\d+)`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scriptPath := filepath.Join(append([]string{root}, tt.path...)...)
			data, err := os.ReadFile(scriptPath)
			if err != nil {
				t.Fatalf("read %s: %v", scriptPath, err)
			}
			matches := tt.pattern.FindAllStringSubmatch(string(data), -1)
			if len(matches) == 0 {
				t.Fatalf("no %s literal found in %s — update this test if the script moved it",
					tt.pattern, scriptPath)
			}
			for _, match := range matches {
				if match[1] != want {
					t.Errorf("%s has %q = %s, want %s (config.DefaultDoltMaxConnections)",
						scriptPath, match[0], match[1], want)
				}
			}
		})
	}
}
