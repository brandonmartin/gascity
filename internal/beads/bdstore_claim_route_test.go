package beads_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreClaimWithMetadataConsumesRouteAtomically verifies that claiming a
// bead while consuming its pool route folds the claim and the metadata mutation
// into a SINGLE bd update invocation: --claim, the --set-metadata for the
// retained anchor, and the --unset-metadata for the consumed route all ride on
// one command so they cannot be observed (or interrupted) separately. This is
// the atomic primitive behind the dispatch double-claim fix (ga-sa0): the route
// is consumed in the same transition that claims the bead.
func TestBdStoreClaimWithMetadataConsumesRouteAtomically(t *testing.T) {
	var calls int
	var gotArgs []string
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			t.Fatalf("name = %q, want bd", name)
		}
		calls++
		gotArgs = append([]string(nil), args...)
		return []byte(`[{"id":"bd-7","title":"pool work","status":"in_progress","assignee":"worker-1","issue_type":"task","created_at":"2026-06-01T00:00:00Z","metadata":{"gc.run_target":"rig/pool"}}]`), nil
	}
	s := beads.NewBdStore("/city", runner)

	claimed, ok, err := s.ClaimWithMetadata("bd-7",
		map[string]string{"gc.run_target": "rig/pool"},
		[]string{"gc.routed_to"})
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("ClaimWithMetadata ok = false, want true")
	}
	if calls != 1 {
		t.Fatalf("bd invocations = %d, want 1 (claim + metadata must be atomic)", calls)
	}
	if claimed.ID != "bd-7" || claimed.Status != "in_progress" || claimed.Assignee != "worker-1" {
		t.Fatalf("claimed bead = %+v, want claimed bd-7 assigned to worker-1", claimed)
	}
	if got := strings.Join(gotArgs, " "); got != "update bd-7 --claim --set-metadata gc.run_target=rig/pool --unset-metadata gc.routed_to --json" {
		t.Fatalf("args = %q, want atomic claim+route-consume args", got)
	}
}

// TestBdStoreClaimWithMetadataMultipleSetKeysSorted verifies set keys are
// emitted in a deterministic (sorted) order so the constructed command is
// stable regardless of map iteration order.
func TestBdStoreClaimWithMetadataMultipleSetKeysSorted(t *testing.T) {
	var gotArgs []string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		gotArgs = append([]string(nil), args...)
		return []byte(`[{"id":"bd-8","status":"in_progress","assignee":"w","issue_type":"task","created_at":"2026-06-01T00:00:00Z"}]`), nil
	}
	s := beads.NewBdStore("/city", runner)

	if _, ok, err := s.ClaimWithMetadata("bd-8",
		map[string]string{"gc.run_target": "p", "z.last": "1", "a.first": "2"},
		nil); err != nil || !ok {
		t.Fatalf("ClaimWithMetadata ok=%v err=%v, want ok=true err=nil", ok, err)
	}
	if got := strings.Join(gotArgs, " "); got != "update bd-8 --claim --set-metadata a.first=2 --set-metadata gc.run_target=p --set-metadata z.last=1 --json" {
		t.Fatalf("args = %q, want sorted --set-metadata flags", got)
	}
}

// TestBdStoreClaimWithMetadataConflictReturnsFalse verifies a claim conflict
// (the bead was already claimed by another actor) yields ok=false with a nil
// error and applies no metadata mutation — mirroring Claim's contract so the
// route is only consumed on a claim this caller actually won.
func TestBdStoreClaimWithMetadataConflictReturnsFalse(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte(`{"error":"issue is already claimed by worker-2"}`), fmt.Errorf("exit status 1")
	}
	s := beads.NewBdStore("/city", runner)

	claimed, ok, err := s.ClaimWithMetadata("bd-9",
		map[string]string{"gc.run_target": "rig/pool"},
		[]string{"gc.routed_to"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("ClaimWithMetadata ok = true, want false on conflict; claimed=%+v", claimed)
	}
}
