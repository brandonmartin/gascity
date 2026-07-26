package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/publishgate"
)

var publishGateNow = time.Date(2026, 7, 26, 6, 0, 0, 0, time.UTC)

// publishGateCheckFor wires a check against an in-memory store and a fake
// repository, with the clock pinned.
func publishGateCheckFor(repo publishgate.Resolver, list []beads.Bead) *publishGateCheck {
	store := beads.NewMemStoreFrom(0, list, nil)
	c := newPublishGateCheck("/rig", "gascity", func(string) (beads.Store, error) { return store, nil })
	c.newResolver = func(string) publishgate.Resolver { return repo }
	c.now = func() time.Time { return publishGateNow }
	return c
}

// heldBead builds a branch-ready bead. readyAt of "" leaves the gate
// without a clock.
func heldBead(id, readyAt string) beads.Bead {
	meta := beads.StringMap{
		publishgate.MetaBranchReady: "true",
		publishgate.MetaBranch:      "polecat/" + id,
		publishgate.MetaCommit:      publishGateTestHead,
		publishgate.MetaTarget:      "develop",
		publishgate.MetaTargetHead:  publishGateTestTarget,
		publishgate.MetaHaltReason:  "mayor_publish_gate",
	}
	if readyAt != "" {
		meta[publishgate.MetaBranchReadyAt] = readyAt
	}
	return beads.Bead{ID: id, Title: "artifact " + id, Type: "task", Status: "open", Metadata: meta}
}

// publishedRepo resolves everything and puts each named bead's artifact at
// its branch tip, so nothing reads as stale.
func publishedRepo(beadIDs ...string) publishgate.Resolver {
	refs := map[string]string{
		publishGateTestHead:           publishGateTestHead,
		"refs/remotes/origin/develop": publishGateTestTarget,
	}
	for _, id := range beadIDs {
		refs["refs/remotes/origin/polecat/"+id] = publishGateTestHead
	}
	return &publishGateFakeRepo{
		refs: refs,
		counts: map[string]int{
			publishGateTestHead + ".." + publishGateTestTarget:   7,
			publishGateTestTarget + ".." + publishGateTestTarget: 0,
		},
	}
}

func TestPublishGateCheckQuietWithNoHolds(t *testing.T) {
	check := publishGateCheckFor(publishedRepo(), []beads.Bead{
		{ID: "ga-1", Title: "ordinary work", Type: "task", Status: "open"},
		{ID: "ga-2", Title: "in flight", Type: "task", Status: "in_progress", Metadata: beads.StringMap{"branch": "polecat/ga-2"}},
	})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK: %#v", res.Status, res)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Errorf("Severity = %v, want Advisory", res.Severity)
	}
	if !strings.Contains(res.Message, "no artifacts held") {
		t.Errorf("Message = %q, want it to report an empty gate", res.Message)
	}
	if len(res.Details) != 0 {
		t.Errorf("Details = %v, want empty", res.Details)
	}
}

func TestPublishGateCheckReportsAFreshHoldWithoutAlarming(t *testing.T) {
	check := publishGateCheckFor(publishedRepo("ga-fresh"),
		[]beads.Bead{heldBead("ga-fresh", publishGateNow.Add(-2*time.Hour).Format(time.RFC3339))})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK for a 2h-old hold: %#v", res.Status, res)
	}
	if !strings.Contains(res.Message, "1 artifact") || !strings.Contains(res.Message, "7 behind develop") {
		t.Errorf("Message = %q, want the count and the measured drift", res.Message)
	}
	if res.FixHint != "" {
		t.Errorf("FixHint = %q, want none while the hold is healthy", res.FixHint)
	}
}

func TestPublishGateCheckWarnsAtOneDay(t *testing.T) {
	check := publishGateCheckFor(publishedRepo("ga-warn"),
		[]beads.Bead{heldBead("ga-warn", publishGateNow.Add(-30*time.Hour).Format(time.RFC3339))})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning: %#v", res.Status, res)
	}
	if res.FixHint == "" {
		t.Error("FixHint should be set once a hold is aging")
	}
	if !strings.Contains(res.Message, "1d6h") {
		t.Errorf("Message = %q, want the gate age", res.Message)
	}
}

func TestPublishGateCheckEscalatesAtThreeDays(t *testing.T) {
	check := publishGateCheckFor(publishedRepo("ga-old"),
		[]beads.Bead{heldBead("ga-old", publishGateNow.Add(-15*24*time.Hour).Format(time.RFC3339))})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error for a 15-day hold: %#v", res.Status, res)
	}
	// Advisory: a rotting artifact is someone's decision to make, not a
	// reason to gate every consumer of doctor.
	if res.Severity != doctor.SeverityAdvisory {
		t.Errorf("Severity = %v, want Advisory", res.Severity)
	}
	details := strings.Join(res.Details, "\n")
	if !strings.Contains(details, "ga-old") || !strings.Contains(details, "15d0h") {
		t.Errorf("Details = %q, want the bead and its age", details)
	}
}

// The check must name the SHA a publisher should ship, and say plainly that
// metadata.branch is not it — the ga-b5h failure.
func TestPublishGateCheckSurfacesUnpublishableBranch(t *testing.T) {
	repo := publishedRepo()
	repo.(*publishGateFakeRepo).refs["refs/remotes/origin/polecat/ga-b5h"] = publishGateTestPreRebase
	check := publishGateCheckFor(repo,
		[]beads.Bead{heldBead("ga-b5h", publishGateNow.Add(-time.Hour).Format(time.RFC3339))})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning when metadata.branch is stale: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	if !strings.Contains(details, publishGateTestPreRebase[:9]) {
		t.Errorf("Details = %q, want the pre-rebase SHA named", details)
	}
	if !strings.Contains(details, "publish the commit, not the branch") {
		t.Errorf("Details = %q, want explicit publish guidance", details)
	}
}

// A gate with no clock sorts ahead of holds with a known, shorter age: an
// unmeasured wait is the more urgent defect.
func TestPublishGateCheckOrdersWorstFirst(t *testing.T) {
	check := publishGateCheckFor(publishedRepo("ga-old", "ga-mid", "ga-nostamp"), []beads.Bead{
		heldBead("ga-mid", publishGateNow.Add(-30*time.Hour).Format(time.RFC3339)),
		heldBead("ga-nostamp", ""),
		heldBead("ga-old", publishGateNow.Add(-10*24*time.Hour).Format(time.RFC3339)),
	})
	res := check.Run(&doctor.CheckContext{})

	if !strings.Contains(res.Message, "3 artifacts") {
		t.Errorf("Message = %q, want all three counted", res.Message)
	}
	if !strings.Contains(res.Message, "ga-old") {
		t.Errorf("Message = %q, want the escalated hold as the headline", res.Message)
	}
	first, second := indexOfBead(res.Details, "ga-nostamp"), indexOfBead(res.Details, "ga-mid")
	if first < 0 || second < 0 || first > second {
		t.Errorf("Details order = %v, want the unmeasured hold above the younger measured one", res.Details)
	}
}

// A halt_reason ending in _gate is a hold even when the halting agent never
// set branch_ready, which an exact-match metadata query cannot express.
func TestPublishGateCheckCatchesGateHaltReasonWithoutBranchReady(t *testing.T) {
	bead := heldBead("ga-gate", publishGateNow.Add(-5*24*time.Hour).Format(time.RFC3339))
	delete(bead.Metadata, publishgate.MetaBranchReady)
	check := publishGateCheckFor(publishedRepo("ga-gate"), []beads.Bead{bead})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusError {
		t.Fatalf("Status = %v, want Error: %#v", res.Status, res)
	}
	if !strings.Contains(res.Message, "ga-gate") {
		t.Errorf("Message = %q, want the gate-halted bead", res.Message)
	}
}

func TestPublishGateCheckDegradesWhenGitIsUnavailable(t *testing.T) {
	check := publishGateCheckFor(&publishGateFakeRepo{},
		[]beads.Bead{heldBead("ga-nogit", publishGateNow.Add(-time.Hour).Format(time.RFC3339))})
	res := check.Run(&doctor.CheckContext{})

	// Unmeasurable drift is a warning, never a crash and never silence.
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning when refs do not resolve: %#v", res.Status, res)
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "does not resolve locally") {
		t.Errorf("Details = %v, want the unresolved target explained", res.Details)
	}
}

func TestPublishGateCheckReportsStoreFailure(t *testing.T) {
	check := newPublishGateCheck("/rig", "gascity", func(string) (beads.Store, error) {
		return nil, fmt.Errorf("dolt unreachable")
	})
	res := check.Run(&doctor.CheckContext{})

	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning: %#v", res.Status, res)
	}
	if !strings.Contains(res.Message, "dolt unreachable") {
		t.Errorf("Message = %q, want the store error surfaced", res.Message)
	}
}

func TestPublishGateCheckNameAndFixability(t *testing.T) {
	check := publishGateCheckFor(publishedRepo(), nil)
	if got := check.Name(); got != "publish-gate:gascity" {
		t.Errorf("Name = %q, want publish-gate:gascity", got)
	}
	if check.CanFix() {
		t.Error("CanFix = true; publish, re-dispatch, and re-stamp are all judgment calls")
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible = true; a days-old hold should not slow gc start")
	}
	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Errorf("Fix = %v, want nil no-op", err)
	}
}

func indexOfBead(details []string, id string) int {
	for i, d := range details {
		if strings.HasPrefix(d, id) {
			return i
		}
	}
	return -1
}
