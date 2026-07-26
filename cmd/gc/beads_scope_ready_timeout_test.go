package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// observedManagedInitCost is the end-to-end cost of a fresh managed-bd city
// init — spawn `dolt sql-server`, run bd's schema migrations, answer the first
// query — measured on a 24-core host at load 58 while other agents' suites ran
// concurrently (ga-df7s). That load is the normal steady state of a Gas City
// host, not an anomaly, so it is the number the readiness ceiling has to clear.
const observedManagedInitCost = 16 * time.Second

// TestBeadsScopeReadyTimeoutClearsObservedManagedInitCost pins the scope-ready
// ceiling above the cost of the work it waits on.
//
// The ceiling is a hang detector, not a latency budget: the wait it bounds
// polls and returns the instant the scope answers, so a larger value costs a
// healthy host nothing. Its only job is to be larger than a legitimate slow
// init. A ceiling sized for an idle machine turns host contention into a test
// failure that cannot be told apart from a real regression, which is exactly
// what made the cmd/gc process suite unreadable as a gate.
//
// This pin exists so nobody re-tightens the ceiling back underneath the
// measured cost of a managed init.
func TestBeadsScopeReadyTimeoutClearsObservedManagedInitCost(t *testing.T) {
	const wantHeadroom = 4
	if floor := wantHeadroom * observedManagedInitCost; beadsScopeReadyTimeout < floor {
		t.Fatalf("beadsScopeReadyTimeout = %s, want >= %s (%dx the %s managed init measured under load in ga-df7s)",
			beadsScopeReadyTimeout, floor, wantHeadroom, observedManagedInitCost)
	}
	if beadsScopeReadyTimeout < testutil.ExecRaceTimeout {
		t.Fatalf("beadsScopeReadyTimeout = %s, below the %s exec-race floor in TESTING.md's test deadline rule",
			beadsScopeReadyTimeout, testutil.ExecRaceTimeout)
	}
}

// TestBeadsScopeReadyTimeoutStaysUnderPackageTestBudget keeps the ceiling from
// growing into a value that hides a wedge instead of reporting it.
//
// A wedged scope must still fail at the call site that wedged, with that call
// site's error message. If the ceiling ever approaches the per-package `go test`
// budget (20-25m for the cmd/gc shards, per TESTING.md), the package timeout
// fires first and the failure arrives as an unattributed stack dump instead.
func TestBeadsScopeReadyTimeoutStaysUnderPackageTestBudget(t *testing.T) {
	// The tightest cmd/gc shard budget is 20m; stay an order of magnitude under
	// it so several sequential waits in one test still report individually.
	const maxCeiling = 2 * time.Minute
	if beadsScopeReadyTimeout > maxCeiling {
		t.Fatalf("beadsScopeReadyTimeout = %s, want <= %s so a wedge reports at its own call site rather than as a package timeout",
			beadsScopeReadyTimeout, maxCeiling)
	}
}
