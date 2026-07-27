package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
)

// liveTrackingRoots returns the non-terminal convoys tracking itemID. Test
// helper mirroring the production live-root lookup so assertions read in the
// same terms as the invariant under test: one live root per tracked bead.
func liveTrackingRoots(t *testing.T, store beads.Store, itemID string) []beads.Bead {
	t.Helper()
	convoys, err := convoycore.TrackingConvoysForItem(store, itemID)
	if err != nil {
		t.Fatalf("TrackingConvoysForItem(%s): %v", itemID, err)
	}
	var live []beads.Bead
	for _, c := range convoys {
		if !convoycore.IsTerminalStatus(c.Status) {
			live = append(live, c)
		}
	}
	return live
}

// clearRoutingState reproduces what a polecat's submit-and-exit does when it
// hands a bead to the refinery, and what the refinery does when it rejects the
// branch and returns the bead to the pool: the routing metadata and assignee
// are cleared while the auto-convoy root stays open.
//
// This is the state a re-slung bead is actually in, and it is precisely the
// state in which CheckBeadStateWithOptions no longer recognizes the bead as
// routed — so the duplicate-convoy guard in resolveConvoyRecovery never runs.
func clearRoutingState(t *testing.T, store beads.Store, beadID string) {
	t.Helper()
	empty := ""
	if err := store.Update(beadID, beads.UpdateOpts{Assignee: &empty}); err != nil {
		t.Fatalf("clearing assignee on %s: %v", beadID, err)
	}
	if err := store.SetMetadata(beadID, "gc.routed_to", ""); err != nil {
		t.Fatalf("clearing gc.routed_to on %s: %v", beadID, err)
	}
}

// A bead that bounces off the refinery and is re-slung must not accumulate a
// second convoy root. The roots are not orphans — they all close together when
// the tracked bead reaches a terminal state — but until then every extra root
// double-counts one piece of in-flight work on the ready board (ga-qar0).
//
// The existing duplicate-convoy guard (needsConvoyRecovery, gastownhall/
// gascity#2987) is keyed on routing state, so it is skipped entirely once
// submit-and-exit has cleared gc.routed_to and the assignee. The reuse must
// therefore live at the mint site, which every dispatch path goes through.
func TestReSlingReusesLiveConvoyRootInsteadOfMintingDuplicate(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if first.ConvoyID == "" {
		t.Fatal("first sling: expected an auto-convoy root")
	}

	clearRoutingState(t, deps.Store, b.ID)

	second, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}

	if second.ConvoyID != first.ConvoyID {
		t.Errorf("re-sling ConvoyID = %q, want %q (the live root must be reused, not replaced)",
			second.ConvoyID, first.ConvoyID)
	}
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 1 {
		ids := make([]string, 0, len(live))
		for _, c := range live {
			ids = append(ids, c.ID)
		}
		t.Errorf("bead %s has %d live convoy roots %v after re-sling, want exactly 1", b.ID, len(live), ids)
	}
}

// Reuse must be scoped to LIVE roots only. Once the tracked bead's prior root
// has closed (the drain path working as designed — it is correct and untouched
// here), a fresh sling of the same bead is a genuinely new dispatch and must
// mint a new root rather than resurrecting the closed one.
func TestSlingMintsFreshRootWhenPriorRootClosed(t *testing.T) {
	runner := newFakeRunner()
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
	deps := testDeps(cfg, runtime.NewFake(), runner.run)

	b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	first, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if first.ConvoyID == "" {
		t.Fatal("first sling: expected an auto-convoy root")
	}

	// The drain closes the root when the tracked bead reaches terminal state.
	if err := deps.Store.Close(first.ConvoyID); err != nil {
		t.Fatalf("closing prior root: %v", err)
	}
	clearRoutingState(t, deps.Store, b.ID)

	second, err := DoSling(SlingOpts{Target: a, BeadOrFormula: b.ID}, deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling DoSling: %v", err)
	}
	if second.ConvoyID == "" {
		t.Fatal("re-sling after closed root: expected a fresh auto-convoy root")
	}
	if second.ConvoyID == first.ConvoyID {
		t.Errorf("re-sling reused closed root %s; a closed root must not be resurrected", first.ConvoyID)
	}
	if live := liveTrackingRoots(t, deps.Store, b.ID); len(live) != 1 {
		t.Errorf("bead %s has %d live convoy roots after re-sling, want exactly 1", b.ID, len(live))
	}
}
