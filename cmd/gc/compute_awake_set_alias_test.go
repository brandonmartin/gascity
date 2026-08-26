package main

import "testing"

// A pool worker claims work under its alias (GC_ALIAS), not its session bead
// ID or runtime session name. The awake set must treat such a claim as
// assigned work or the reconciler drains the claimed session as orphaned
// once its spawning demand is consumed (ga-9wh, ga-ei0).
func TestSessionAssigneeMatchesPoolAlias(t *testing.T) {
	bead := AwakeSessionBead{
		ID:          "bo-abc123",
		SessionName: "gascity--gasburger__anvil",
		Template:    "gascity/gasburger.clodcats",
		Alias:       "gascity/gasburger.anvil",
	}
	if !sessionAssigneeMatches(nil, bead, "gascity/gasburger.anvil") {
		t.Fatalf("alias assignee must match the pool session bead")
	}
	if sessionAssigneeMatches(nil, bead, "gascity/gasburger.girder") {
		t.Fatalf("a different pool alias must not match")
	}
	work := []AwakeWorkBead{{ID: "ga-ei0", Assignee: "gascity/gasburger.anvil", Status: "in_progress"}}
	if !sessionHasAssignedWork(work, nil, bead) {
		t.Fatalf("claimed in_progress work under the alias must count as assigned work")
	}
	// An unaliased session bead keeps the pre-existing behaviour.
	bead.Alias = ""
	if sessionAssigneeMatches(nil, bead, "gascity/gasburger.anvil") {
		t.Fatalf("no alias, no match")
	}
}
