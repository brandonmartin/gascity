package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// A worker that cleared both gc.routed_to and assignee in a failed done
// sequence (e.g. $REFINERY_TARGET was empty) leaves the work bead
// open+unassigned+unrouted with a branch on origin. The pool demand probe
// can't see it (keys on gc.routed_to) and releaseOrphanedPoolAssignments
// can't recover it (only processes assigned work). sweepDetachedHandoffOrphans
// finds it via gc.session_name → session bead → template.
func TestSweepDetachedHandoffOrphans_RestoresRoute(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-abc",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphaned work",
		Status: "open",
		Metadata: map[string]string{
			"branch":                          "polecat/ga-abc",
			beadmeta.SessionNameMetadataKey:   sessionBead.Metadata["session_name"],
			// gc.routed_to and assignee left empty by failed done sequence
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if got.Assignee != "" {
		t.Fatalf("assignee=%q, want empty", got.Assignee)
	}
	if got.Status != "open" {
		t.Fatalf("status=%q, want open", got.Status)
	}
}

// A bead that still has gc.routed_to set must not be touched.
func TestSweepDetachedHandoffOrphans_SkipsAlreadyRouted(t *testing.T) {
	store := beads.NewMemStore()

	work, err := store.Create(beads.Bead{
		Title:  "routed work",
		Status: "open",
		Metadata: map[string]string{
			"branch":                          "polecat/ga-routed",
			beadmeta.SessionNameMetadataKey:   "some-session",
			beadmeta.RoutedToMetadataKey:      "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to changed unexpectedly to %q", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A bead that still has an assignee must not be touched (covered by
// releaseOrphanedPoolAssignments when the session closes).
func TestSweepDetachedHandoffOrphans_SkipsAssigned(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:    "assigned work",
		Status:   "open",
		Assignee: "some-session-id",
		Metadata: map[string]string{
			"branch":                        "polecat/ga-assigned",
			beadmeta.SessionNameMetadataKey: "some-session-id",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (assigned beads are not detached orphans)", n)
	}
}

// Workflow-kind beads route via gc.run_target and are handled by
// restoreCarriedWorkRoutes — the detached-orphan sweep must leave them alone.
func TestSweepDetachedHandoffOrphans_SkipsWorkflowKind(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "workflow root",
		Status: "open",
		Metadata: map[string]string{
			"branch":                        "polecat/ga-wf",
			beadmeta.SessionNameMetadataKey: "some-session",
			beadmeta.KindMetadataKey:        beadmeta.KindWorkflow,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (workflow-kind beads are not detached handoff orphans)", n)
	}
}

// A bead with no branch set is not a completed-work bead and must be skipped.
func TestSweepDetachedHandoffOrphans_SkipsNoBranch(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "no-branch bead",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.SessionNameMetadataKey: "some-session",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (no branch → not a completed-work bead)", n)
	}
}

// A bead with no gc.session_name cannot be routed back to the pool because
// we have no session reference to recover the route from.
func TestSweepDetachedHandoffOrphans_SkipsNoSessionName(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "no-session work",
		Status: "open",
		Metadata: map[string]string{
			"branch": "polecat/ga-nosession",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (no gc.session_name → can't recover route)", n)
	}
}

// When the session bead is not found (e.g. already GC'd), the bead is left
// untouched — no route to recover.
func TestSweepDetachedHandoffOrphans_SkipsWhenSessionNotFound(t *testing.T) {
	store := beads.NewMemStore()

	work, err := store.Create(beads.Bead{
		Title:  "orphan with missing session",
		Status: "open",
		Metadata: map[string]string{
			"branch":                        "polecat/ga-gone",
			beadmeta.SessionNameMetadataKey: "gastown__polecat-gone",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (session bead not found)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty (no session bead to recover from)", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// When the session bead exists but has no template/agent_name, no route can be
// recovered — the bead is left untouched but not an error.
func TestSweepDetachedHandoffOrphans_SkipsWhenSessionHasNoTemplate(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "templateless session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "unknown-session",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphan with templateless session",
		Status: "open",
		Metadata: map[string]string{
			"branch":                        "polecat/ga-notempl",
			beadmeta.SessionNameMetadataKey: "unknown-session",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 (session has no template)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("gc.routed_to=%q, want empty", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// A closed session bead still carries the template we need — route recovery
// works even after the worker session has been closed and GC-swept.
func TestSweepDetachedHandoffOrphans_RecoverFromClosedSessionBead(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "closed polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-closed",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	// Close the session bead to simulate the dead-worker scenario.
	closed := "closed"
	if err := store.Update(sessionBead.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close session bead: %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:  "orphaned work (closed session)",
		Status: "open",
		Metadata: map[string]string{
			"branch":                        "polecat/ga-closed",
			beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("restored=%d, want 1 (closed session bead still carries template)", n)
	}

	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to=%q, want gascity/gastown.polecat", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
}

// Multiple detached orphans from the same session are all recovered in one sweep.
func TestSweepDetachedHandoffOrphans_MultipleCandidates(t *testing.T) {
	store := beads.NewMemStore()

	sessionBead, err := store.Create(beads.Bead{
		Title:  "polecat session",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "gastown__polecat-th-multi",
			"template":     "gascity/gastown.polecat",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{
			Status: "open",
			Metadata: map[string]string{
				"branch":                        "polecat/ga-multi",
				beadmeta.SessionNameMetadataKey: sessionBead.Metadata["session_name"],
			},
		}); err != nil {
			t.Fatalf("create work bead %d: %v", i, err)
		}
	}

	n, err := sweepDetachedHandoffOrphans(store)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 3 {
		t.Fatalf("restored=%d, want 3", n)
	}
}

// Nil store is a no-op.
func TestSweepDetachedHandoffOrphans_NilStore(t *testing.T) {
	n, err := sweepDetachedHandoffOrphans(nil)
	if err != nil {
		t.Fatalf("sweep nil store: %v", err)
	}
	if n != 0 {
		t.Fatalf("restored=%d, want 0 for nil store", n)
	}
}
