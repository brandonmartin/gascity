package scripts_test

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFixtureSpawnedChildrenNeverQueueOnThePushGate pins the invariant that
// broke `make test` in ga-8yfu: a fixture-spawned child must acquire a
// push-gate slot immediately even when every slot is already held.
//
// Fixture children shell out to `make`/scripts/test-go-test-shard, both of
// which funnel into scripts/go-test-observable and acquire a slot whenever a
// city root resolves. Because a fixture builds an explicit env whitelist, the
// GC_PUSH_GATE_HELD marker its parent exported is dropped, and the child queues
// against the very sweep that spawned it — a self-deadlock that surfaces as an
// opaque 15-minute package timeout rather than a test failure.
//
// The probe drives the real library against a temporary slot directory, so it
// never touches the city's live slots.
func TestFixtureSpawnedChildrenNeverQueueOnThePushGate(t *testing.T) {
	t.Parallel()

	requirePushGateProbeTools(t)

	fixture := newGoTestShardFixture(t)
	for _, tc := range []struct {
		name string
		env  []string
	}{
		{name: "shard command", env: fixture.command().Env},
		{name: "timing isolation", env: fixture.timingIsolationEnv(filepath.Join(fixture.tmpDir, "timing.json"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			output, err := probePushGateAcquire(t, tc.env)
			if err != nil || !strings.Contains(output, "ACQUIRED") {
				t.Fatalf("fixture child queued for a push-gate slot instead of bypassing the cap: %v\n%s\n"+
					"the child env must carry %s, or a sweep holding every slot deadlocks against its own child",
					err, output, pushGateFixtureOptOut)
			}
		})
	}
}

// TestPushGateMarkersSurviveTestEnvScrubs pins the push-gate env contract
// across the two `env -i` boundaries between an outer sweep and the nested
// scripts/go-test-observable it eventually execs.
//
// Both scrubs are allowlists, and the gate's two pieces of cross-boundary
// state are carried by nothing but exported variables:
//
//   - GC_PUSH_GATE_HELD signals "this process tree already holds a slot". A
//     marker dropped at either boundary re-arms the cap inside a tree that
//     already holds one, and the failure is silent: the nested runner queues
//     behind its own parent until the wait bound expires, surfacing as a
//     package timeout rather than anything naming the gate.
//   - GC_PUSH_GATE_CITY_ROOT names the city whose pool to contend for. The
//     scrubs drop GC_CITY_PATH/GC_CITY/GC_CITY_ROOT on purpose (ga-w2kh1r),
//     which leaves push_gate_city_root only the $PWD walk-up — so a caller
//     whose worktree sits outside the city tree silently resolves a
//     *repo-level* pool instead. Two disjoint pools then each enforce
//     PUSH_GATE_MAX_CONCURRENT and the town-wide cap doubles (ga-x36q).
//     Dropping this forward is equally silent: the cap still appears to be
//     configured, it just stops being one cap.
func TestPushGateMarkersSurviveTestEnvScrubs(t *testing.T) {
	t.Parallel()

	// The assignment form, not a bare mention — the surrounding comments name
	// these variables too, and only the assignment actually forwards them.
	for _, scrub := range []struct {
		path     string
		forwards []string
	}{
		{
			path: "Makefile",
			forwards: []string{
				`GC_PUSH_GATE_HELD="$${GC_PUSH_GATE_HELD-}"`,
				`GC_PUSH_GATE_NO_CAP="$${GC_PUSH_GATE_NO_CAP-}"`,
				`GC_PUSH_GATE_CITY_ROOT="$${GC_PUSH_GATE_CITY_ROOT:-$${GC_CITY_PATH:-$${GC_CITY:-$${GC_CITY_ROOT-}}}}"`,
			},
		},
		{
			// Appended to observable_env, not base_env: only the
			// go-test-observable exec consults the gate, and the direct
			// `go test` product environment is a curated contract that
			// TestGoTestShardWithoutTimingPreservesDirectProductContract pins.
			path: filepath.Join("scripts", "test-go-test-shard"),
			forwards: []string{
				`observable_env+=("GC_PUSH_GATE_HELD=${GC_PUSH_GATE_HELD}")`,
				`observable_env+=("GC_PUSH_GATE_NO_CAP=${GC_PUSH_GATE_NO_CAP}")`,
				`observable_env+=("GC_PUSH_GATE_CITY_ROOT=${GC_PUSH_GATE_CITY_ROOT}")`,
			},
		},
	} {
		t.Run(scrub.path, func(t *testing.T) {
			t.Parallel()

			content, err := os.ReadFile(filepath.Join(repoRoot(t), scrub.path))
			if err != nil {
				t.Fatalf("read %s: %v", scrub.path, err)
			}
			for _, forward := range scrub.forwards {
				if !strings.Contains(string(content), forward) {
					t.Errorf("%s scrubs the environment without forwarding %s\n"+
						"the gate then silently mis-scopes: a nested runner re-acquires a slot it "+
						"already holds and blocks against its own parent, or a caller outside the "+
						"city tree contends in a second, disjoint pool",
						scrub.path, forward)
				}
			}
		})
	}
}

// requirePushGateProbeTools skips when the probe cannot distinguish a pass from
// a failure. push_gate_acquire_slot degrades to an immediate success when
// flock(1) is missing, which would make this test vacuously green.
func requirePushGateProbeTools(t *testing.T) {
	t.Helper()

	for _, tool := range []string{"bash", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("push-gate probe needs %s: %v", tool, err)
		}
	}
}

// probePushGateAcquire holds the only slot in a throwaway slot directory, then
// asks push_gate_acquire_slot for one using childEnv. It prints ACQUIRED only
// when the child bypasses the cap outright; a child that honors the cap waits
// out the (zeroed) bound and reports failure instead of hanging the suite.
func probePushGateAcquire(t *testing.T, childEnv []string) (string, error) {
	t.Helper()

	slotDir := filepath.Join(t.TempDir(), "gate-slots")
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		t.Fatalf("create probe slot directory: %v", err)
	}
	holdPushGateSlot(t, filepath.Join(slotDir, "slot-0.lock"))

	probe := exec.Command(
		"bash", "-c",
		`source "$1" || exit 90
push_gate_acquire_slot "$2" GATE_FD fixture-child || exit 91
printf ACQUIRED`,
		"push-gate-probe",
		filepath.Join(repoRoot(t), "scripts", "push-gate-lock-lib.sh"),
		slotDir,
	)
	// The tunables bound the probe itself, so a child that does queue fails
	// fast instead of inheriting go-test-observable's 1800s wait. They are
	// deliberately not part of the fixture whitelist under test.
	probe.Env = append(append([]string(nil), childEnv...),
		"PUSH_GATE_MAX_CONCURRENT=1",
		"PUSH_GATE_MAX_WAIT_SECONDS=0",
		"PUSH_GATE_POLL_SECONDS=1",
	)

	output, err := probe.CombinedOutput()
	return string(output), err
}

// holdPushGateSlot locks slotFile for the rest of the test, so the probe faces
// a fully occupied slot directory.
//
// The holder announces ownership on stdout the moment flock(1) hands it the
// lock, and only then replaces itself with the long sleep that keeps the slot
// occupied. Waiting on that marker is a lifecycle signal, not elapsed wall
// time: it costs nothing on the happy path, and the timeout below fires only
// when the holder genuinely fails to lock.
func holdPushGateSlot(t *testing.T, slotFile string) {
	t.Helper()

	const heldMarker = "HELD"

	holder := exec.Command("flock", slotFile, "bash", "-c", "printf "+heldMarker+"; exec sleep 300")
	announced, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("pipe push-gate slot holder stdout: %v", err)
	}
	if err := holder.Start(); err != nil {
		t.Fatalf("start push-gate slot holder: %v", err)
	}
	t.Cleanup(func() {
		_ = holder.Process.Kill()
		_ = holder.Wait()
	})

	held := make(chan error, 1)
	go func() {
		marker := make([]byte, len(heldMarker))
		if _, err := io.ReadFull(announced, marker); err != nil {
			held <- err
			return
		}
		if string(marker) != heldMarker {
			held <- fmt.Errorf("announced %q, want %q", marker, heldMarker)
			return
		}
		held <- nil
	}()

	select {
	case err := <-held:
		if err != nil {
			t.Fatalf("push-gate slot holder never locked %s: %v", slotFile, err)
		}
		// The holder owns the lock; the probe now faces a full slot set.
	case <-time.After(30 * time.Second):
		t.Fatalf("push-gate slot holder never locked %s within 30s", slotFile)
	}
}
