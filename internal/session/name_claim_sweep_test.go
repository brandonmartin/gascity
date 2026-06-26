package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func staleClaimSweepConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "refinery",
			BindingName: "gastown",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "refinery",
			BindingName: "gastown",
		}},
	}
}

// TestReleaseStaleConfiguredNameClaims_ReleasesClosedLegacyClaim covers ga-n2d
// Gap C: a CLOSED pre-ga841 phantom that reserves a configured runtime name and
// carries its identity only via the alias/template (role) label has its stale
// name claim released at startup, so the post-fix binary begins from a clean
// name index.
func TestReleaseStaleConfiguredNameClaims_ReleasesClosedLegacyClaim(t *testing.T) {
	store := beads.NewMemStore()
	cfg := staleClaimSweepConfig()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "gastown.refinery")
	if runtimeName == "" {
		t.Fatal("precondition: empty runtime name for gastown.refinery")
	}

	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": runtimeName,
			// Legacy identity signal only — no flag, no configured_named_identity.
			"alias":    "gastown.refinery",
			"template": "gastown.refinery",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	released, err := ReleaseStaleConfiguredNameClaims(store, cfg, "test-city")
	if err != nil {
		t.Fatalf("ReleaseStaleConfiguredNameClaims: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if sn := got.Metadata["session_name"]; sn != "" {
		t.Fatalf("session_name = %q, want empty after sweep", sn)
	}
}

// TestReleaseStaleConfiguredNameClaims_ReleasesClosedFlaggedClaim confirms the
// sweep also clears properly-tagged closed named sessions (flag + identity), not
// just legacy beads.
func TestReleaseStaleConfiguredNameClaims_ReleasesClosedFlaggedClaim(t *testing.T) {
	store := beads.NewMemStore()
	cfg := staleClaimSweepConfig()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "gastown.refinery")

	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":              runtimeName,
			"configured_named_session":  "true",
			"configured_named_identity": "gastown.refinery",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	released, err := ReleaseStaleConfiguredNameClaims(store, cfg, "test-city")
	if err != nil {
		t.Fatalf("ReleaseStaleConfiguredNameClaims: %v", err)
	}
	if released != 1 {
		t.Fatalf("released = %d, want 1", released)
	}
}

// TestReleaseStaleConfiguredNameClaims_PreservesLiveClaim guards against
// disrupting a session that may still own or resume its name: only CLOSED beads
// are swept. A live (open) bead holding the configured runtime name keeps it.
func TestReleaseStaleConfiguredNameClaims_PreservesLiveClaim(t *testing.T) {
	store := beads.NewMemStore()
	cfg := staleClaimSweepConfig()
	runtimeName := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "gastown.refinery")

	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":              runtimeName,
			"configured_named_session":  "true",
			"configured_named_identity": "gastown.refinery",
			"state":                     string(StateAsleep),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	released, err := ReleaseStaleConfiguredNameClaims(store, cfg, "test-city")
	if err != nil {
		t.Fatalf("ReleaseStaleConfiguredNameClaims: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want 0 (live bead must keep its name)", released)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if sn := got.Metadata["session_name"]; sn != runtimeName {
		t.Fatalf("session_name = %q, want %q (unchanged)", sn, runtimeName)
	}
}

// TestReleaseStaleConfiguredNameClaims_IgnoresUnconfiguredName confirms a closed
// bead whose reserved name is NOT a configured named-session runtime name (an
// ad-hoc session) is left untouched, preserving the ad-hoc permanent-name
// guarantee.
func TestReleaseStaleConfiguredNameClaims_IgnoresUnconfiguredName(t *testing.T) {
	store := beads.NewMemStore()
	cfg := staleClaimSweepConfig()

	bead, err := store.Create(beads.Bead{
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "my-custom-session",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	released, err := ReleaseStaleConfiguredNameClaims(store, cfg, "test-city")
	if err != nil {
		t.Fatalf("ReleaseStaleConfiguredNameClaims: %v", err)
	}
	if released != 0 {
		t.Fatalf("released = %d, want 0 (ad-hoc name must be preserved)", released)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if sn := got.Metadata["session_name"]; sn != "my-custom-session" {
		t.Fatalf("session_name = %q, want unchanged", sn)
	}
}
