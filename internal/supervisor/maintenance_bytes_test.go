package supervisor

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// sizeProbe returns a StoreSizeBytes func that yields the scripted values
// in order, repeating the final value once exhausted. It lets a test
// distinguish the before-cycle measurement from the after-cycle one.
func sizeProbe(values ...int64) func() int64 {
	var idx int
	return func() int64 {
		v := values[idx]
		if idx < len(values)-1 {
			idx++
		}
		return v
	}
}

// TestExecuteCycle_PopulatesBeforeAfterBytes verifies a successful cycle
// records the store size measured before the run and after the GC, so a
// no-op (before == after) is detectable from the run record. Regression
// guard for ga-ule: the fields existed end-to-end but were never set.
func TestExecuteCycle_PopulatesBeforeAfterBytes(t *testing.T) {
	ops := &fakeDoltOps{smokeCount: 5}
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:            config.DoltMaintenance{Enabled: true},
		CityPath:       t.TempDir(),
		OpenDoltOps:    func(context.Context) (DoltOps, error) { return ops, nil },
		StoreSizeBytes: sizeProbe(8_000_000_000, 250_000_000),
	})

	run, err := loop.TriggerNow(context.Background())
	if err != nil {
		t.Fatalf("TriggerNow: %v", err)
	}
	if run.Stage != "done" {
		t.Fatalf("Stage = %q, want done; err=%q", run.Stage, run.Err)
	}
	if run.BeforeBytes != 8_000_000_000 {
		t.Errorf("BeforeBytes = %d, want 8000000000", run.BeforeBytes)
	}
	if run.AfterBytes != 250_000_000 {
		t.Errorf("AfterBytes = %d, want 250000000", run.AfterBytes)
	}
}

// TestExecuteCycle_BeforeAfterBytesOnFailure verifies the size
// measurements are recorded even when the cycle fails, so operators can
// see the store did not shrink.
func TestExecuteCycle_BeforeAfterBytesOnFailure(t *testing.T) {
	ops := &fakeDoltOps{smokeCount: 0} // smoke failure
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:            config.DoltMaintenance{Enabled: true},
		CityPath:       t.TempDir(),
		OpenDoltOps:    func(context.Context) (DoltOps, error) { return ops, nil },
		StoreSizeBytes: sizeProbe(8_000_000_000),
	})

	run, err := loop.TriggerNow(context.Background())
	if err != nil {
		t.Fatalf("TriggerNow: %v", err)
	}
	if run.Stage != "smoke-test" {
		t.Fatalf("Stage = %q, want smoke-test", run.Stage)
	}
	if run.BeforeBytes != 8_000_000_000 || run.AfterBytes != 8_000_000_000 {
		t.Errorf("before/after = %d/%d, want 8000000000/8000000000", run.BeforeBytes, run.AfterBytes)
	}
}

// TestRunOnce_ScheduledPathRunsSameGCAsManual is the regression guard for
// ga-ule ask 3: the in-supervisor weekly loop (runOnce, fired by the
// scheduler) must execute the same GC path as a manual TriggerNow — not the
// silent no-op. Both funnel through executeCycleLocked, so a wired
// OpenDoltOps reclaims on the scheduled tick too.
func TestRunOnce_ScheduledPathRunsSameGCAsManual(t *testing.T) {
	ops := &fakeDoltOps{smokeCount: 5}
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:         config.DoltMaintenance{Enabled: true},
		CityPath:    t.TempDir(),
		OpenDoltOps: func(context.Context) (DoltOps, error) { return ops, nil },
	})

	loop.runOnce(context.Background())

	if ops.gcCalls != 1 {
		t.Errorf("scheduled runOnce ran ExecGC %d times; want 1 (same path as manual trigger)", ops.gcCalls)
	}
	hist := loop.History()
	if len(hist) != 1 || hist[0].Stage != "done" {
		t.Fatalf("history = %+v; want exactly one done run", hist)
	}
}

// TestExecuteCycle_NilSizeProbeLeavesBytesZero confirms the probe is
// optional: an unset StoreSizeBytes leaves the fields at the documented
// "0 == not measured" sentinel without panicking.
func TestExecuteCycle_NilSizeProbeLeavesBytesZero(t *testing.T) {
	ops := &fakeDoltOps{smokeCount: 5}
	loop := NewStoreMaintenanceLoop(StoreMaintenanceLoopDeps{
		Cfg:         config.DoltMaintenance{Enabled: true},
		CityPath:    t.TempDir(),
		OpenDoltOps: func(context.Context) (DoltOps, error) { return ops, nil },
	})

	run, err := loop.TriggerNow(context.Background())
	if err != nil {
		t.Fatalf("TriggerNow: %v", err)
	}
	if run.BeforeBytes != 0 || run.AfterBytes != 0 {
		t.Errorf("before/after = %d/%d, want 0/0", run.BeforeBytes, run.AfterBytes)
	}
}
