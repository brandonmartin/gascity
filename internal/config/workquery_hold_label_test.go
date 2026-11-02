package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// This file expresses the ga-x9kptu / ga-5736js acceptance criteria at the
// shell-generator level: route-scoped, unassigned pool-demand queries (Tier
// 3, and the reconciler's count-form) must exclude beads carrying a
// beadmeta.DispatchHoldLabels value and the reserved hold:<reason> namespace,
// while the assignee-scoped ephemeral probe (Tier 1/2) stays hold-transparent.

func TestBdReadyPoolDemandShellExcludesDispatchHoldLabels(t *testing.T) {
	got := bdReadyPoolDemandShell("--limit 0", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("bdReadyPoolDemandShell() = %q, missing %q", got, want)
		}
	}
}

func TestBdReadyPoolDemandMigrationShellExcludesDispatchHoldLabels(t *testing.T) {
	got := bdReadyPoolDemandMigrationShell("--limit=20", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("bdReadyPoolDemandMigrationShell() = %q, missing %q", got, want)
		}
	}
}

func TestLegacyEphemeralPoolDemandShellRouteScopedExcludesDispatchHoldLabels(t *testing.T) {
	got := legacyEphemeralPoolDemandShell(20, QueryTopology{})
	if !strings.Contains(got, ".labels") {
		t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing a .labels reference", got)
	}
	for _, label := range beadmeta.DispatchHoldLabels {
		if !strings.Contains(got, `"`+label+`"`) {
			t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing hold label %q", got, label)
		}
	}
}

func TestEphemeralAssignedReadyProbeScriptDoesNotExcludeDispatchHoldLabels(t *testing.T) {
	got := ephemeralAssignedReadyProbeScript("cand", QueryTopology{})
	if strings.Contains(got, "--exclude-label") || strings.Contains(got, ".labels") {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, assignee-scoped tier must stay hold-transparent", got)
	}
}

func TestEffectiveRoutedPoolQueryCarriesHoldLabelExclusionForLegacyAlias(t *testing.T) {
	a := &Agent{Name: ControlDispatcherAgentName, Dir: "rig"}
	got := a.EffectiveRoutedPoolQuery()
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("EffectiveRoutedPoolQuery() (legacy-alias agent) = %q, missing %q", got, want)
		}
	}
}

func TestPoolDemandCountShellInheritsDispatchHoldLabelExclusion(t *testing.T) {
	got := poolDemandCountShell("hello-world/worker", QueryTopology{})
	for _, label := range beadmeta.DispatchHoldLabels {
		want := `--exclude-label "` + label + `"`
		if !strings.Contains(got, want) {
			t.Errorf("poolDemandCountShell() = %q, missing %q (reconciler count-form must inherit the claim-path fix)", got, want)
		}
	}
}

// bd's native human label parks a bead for a person. It is not a dispatch hold:
// beadmeta.DispatchHoldLabels names the two canonical values for bd's exact
// label flags. The hook also recognizes the hold:<reason> namespace, so each
// route-scoped query renders from those rules. Assignee-scoped probes must stay
// transparent to the separate human label.

func TestPoolDemandServeRulesExcludeHumanLabelWithoutMakingItADispatchHold(t *testing.T) {
	if rules := PoolDemandServeRulesForQuery(); !slices.Contains(rules.ExcludeLabels, "human") {
		t.Errorf("PoolDemandServeRulesForQuery().ExcludeLabels = %v, missing the human label", rules.ExcludeLabels)
	}
	if rules := PoolDemandServeRulesForQuery(); !slices.Contains(rules.ExcludeLabelPrefixes, "hold:") {
		t.Errorf("PoolDemandServeRulesForQuery().ExcludeLabelPrefixes = %v, missing the hold namespace", rules.ExcludeLabelPrefixes)
	}
	if slices.Contains(beadmeta.DispatchHoldLabels, "human") {
		t.Errorf("beadmeta.DispatchHoldLabels = %v, must not name the human label: it is not a dispatch hold", beadmeta.DispatchHoldLabels)
	}
}

func TestRoutedAndCountQueriesExcludeTheWholeHoldNamespace(t *testing.T) {
	requireJQ(t)
	a := &Agent{Name: "worker", Dir: "hello-world"}
	fakeBD := `#!/bin/sh
set -eu
case "$1" in
  ready)
    case "$*" in
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '%s' '[{"id":"held","status":"open","labels":["hold:mayor-go"]},{"id":"ready","status":"open","labels":["task"]}]'
        ;;
      *) printf '[]' ;;
    esac
    ;;
  query|list|show) printf '[]' ;;
  *) printf '[]' ;;
esac
`

	work := runShellWithFakeBd(t, a.EffectiveWorkQuery(), map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
	}, fakeBD)
	if got := workQueryOutputIDOrder(t, strings.TrimSpace(work)); len(got) != 1 || got[0] != "ready" {
		t.Errorf("EffectiveWorkQuery() ids = %v, want only ready; a hold:mayor-go row must not be claimable", got)
	}

	demand := runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), nil, fakeBD)
	if got := strings.TrimSpace(demand); got != "1" {
		t.Errorf("EffectivePoolDemandQuery() count = %q, want 1 (only the unheld ready row)", got)
	}
}

// `bd set-state <id> hold=cleared` releases a hold and leaves hold:cleared
// behind (ga-azdbu). That label sits inside the reserved hold: namespace but
// is "no hold": the routed and count queries must keep serving and counting
// the row, and the descriptor must carry the exception so the controller's
// demand predicate renders from the same source.

func TestPoolDemandServeRulesCarryTheReleasedHoldException(t *testing.T) {
	rules := PoolDemandServeRulesForQuery()
	if !slices.Equal(rules.ExcludeLabelPrefixExceptions, beadmeta.DispatchHoldReleasedLabels) {
		t.Errorf("PoolDemandServeRulesForQuery().ExcludeLabelPrefixExceptions = %v, want %v",
			rules.ExcludeLabelPrefixExceptions, beadmeta.DispatchHoldReleasedLabels)
	}
	for _, label := range rules.ExcludeLabels {
		if slices.Contains(beadmeta.DispatchHoldReleasedLabels, label) {
			t.Errorf("ExcludeLabels names released label %q; bd's exact --exclude-label flag would park it", label)
		}
	}
}

func TestRoutedAndCountQueriesServeAReleasedHold(t *testing.T) {
	requireJQ(t)
	a := &Agent{Name: "worker", Dir: "hello-world"}
	cases := []struct {
		name   string
		labels string
		served bool
	}{
		{"released hold", `["hold:cleared"]`, true},
		{"released hold case folded", `["Hold:Cleared"]`, true},
		{"released hold beside ordinary label", `["task","hold:cleared"]`, true},
		{"released then re-held", `["hold:cleared","hold:mayor-go"]`, false},
		{"re-held then released", `["hold:mayor-go","hold:cleared"]`, false},
		{"released-label lookalike", `["hold:cleared-by-mayor"]`, false},
		{"canonical hold", `["hold:mayor"]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeBD := `#!/bin/sh
set -eu
case "$1" in
  ready)
    case "$*" in
      *"--metadata-field gc.routed_to=hello-world/worker"*)
        printf '%s' '[{"id":"row","status":"open","labels":` + tc.labels + `}]'
        ;;
      *) printf '[]' ;;
    esac
    ;;
  *) printf '[]' ;;
esac
`
			wantIDs, wantCount := []string(nil), "0"
			if tc.served {
				wantIDs, wantCount = []string{"row"}, "1"
			}

			work := strings.TrimSpace(runShellWithFakeBd(t, a.EffectiveWorkQuery(), map[string]string{
				"GC_SESSION_ORIGIN": "ephemeral",
			}, fakeBD))
			var gotIDs []string
			if work != "" {
				gotIDs = workQueryOutputIDOrder(t, work)
			}
			if !slices.Equal(gotIDs, wantIDs) {
				t.Errorf("EffectiveWorkQuery() ids = %v, want %v for labels %s", gotIDs, wantIDs, tc.labels)
			}

			demand := strings.TrimSpace(runShellWithFakeBd(t, a.EffectivePoolDemandQuery(), nil, fakeBD))
			if demand != wantCount {
				t.Errorf("EffectivePoolDemandQuery() count = %q, want %q for labels %s", demand, wantCount, tc.labels)
			}
		})
	}
}

func TestRouteScopedPoolDemandShellsExcludeHumanLabel(t *testing.T) {
	want := `--exclude-label "human"`
	for name, got := range map[string]string{
		"bdReadyPoolDemandShell":          bdReadyPoolDemandShell("--limit 0", QueryTopology{}),
		"bdReadyPoolDemandMigrationShell": bdReadyPoolDemandMigrationShell("--limit=20", QueryTopology{}),
		"poolDemandCountShell":            poolDemandCountShell("hello-world/worker", QueryTopology{}),
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s() = %q, missing %q", name, got, want)
		}
	}
	if got := legacyEphemeralPoolDemandShell(20, QueryTopology{}); !strings.Contains(got, `"human"`) {
		t.Errorf("legacyEphemeralPoolDemandShell() = %q, missing the human label in its jq label filter", got)
	}
}

func TestAssigneeScopedProbesStayHumanTransparent(t *testing.T) {
	for name, got := range map[string]string{
		"ephemeralAssignedReadyProbeScript":      ephemeralAssignedReadyProbeScript("cand", QueryTopology{}),
		"ephemeralAssignedInProgressProbeScript": ephemeralAssignedInProgressProbeScript("cand", QueryTopology{}),
	} {
		if strings.Contains(got, `"human"`) {
			t.Errorf("%s() = %q, assignee-scoped tier must stay human-transparent", name, got)
		}
	}
}
