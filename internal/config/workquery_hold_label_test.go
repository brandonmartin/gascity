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
// beadmeta.DispatchHoldLabels value and the reserved hold:<reason> namespace.
// The generated assignee-scoped tiers serve work as well, so they exclude the
// dispatch holds too but never the human label; the assigned-ready tier's
// exclusion (#8010) is pinned in workquery_assigned_ready_hold_test.go.

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

// The bd-1.0.4 assigned-ready wisp probe serves its first row and exits the
// work query, so its jq selection drops the hold: namespace (#8010); the
// behavior is pinned in workquery_assigned_ready_hold_test.go.
func TestEphemeralAssignedReadyProbeScriptExcludesDispatchHoldLabels(t *testing.T) {
	got := ephemeralAssignedReadyProbeScript("cand", QueryTopology{})
	if !strings.Contains(got, ".labels") {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, missing a .labels reference", got)
	}
	if !strings.Contains(got, `startswith("`+beadmeta.DispatchHoldLabelPrefix+`")`) {
		t.Errorf("ephemeralAssignedReadyProbeScript() = %q, missing the %q prefix filter", got, beadmeta.DispatchHoldLabelPrefix)
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
		"ephemeralAssignedReadyProbeScript":       ephemeralAssignedReadyProbeScript("cand", QueryTopology{}),
		"ephemeralAssignedInProgressProbeScript":  ephemeralAssignedInProgressProbeScript("cand", QueryTopology{}),
		"assignedReadyTierCommand/bd104":          assignedReadyTierCommand("cand", QueryTopology{}),
		"assignedReadyTierCommand/bd105":          assignedReadyTierCommand("cand", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}}),
		"assignedReadyTierCommand/bd105federated": assignedReadyTierCommand("cand", QueryTopology{Beads: BeadsConfig{BDCompatibility: BeadsBDCompatibility105}, FederatedReady: true}),
	} {
		if strings.Contains(got, `"human"`) {
			t.Errorf("%s() = %q, assignee-scoped tier must stay human-transparent", name, got)
		}
	}
}
