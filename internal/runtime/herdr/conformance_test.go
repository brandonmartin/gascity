//go:build integration

package herdr

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// TestHerdrConformance runs the full runtime.Provider conformance suite against
// the production herdr constructor. Each factory call uses its own isolated
// herdr session-server so session-scoped assertions (ListRunning, orphan
// detection, …) do not observe sibling sessions.
//
// The test is integration-tagged so the unit lane does not compile it (the
// ledger forbids pre-run Skip in the proof function itself). When this file
// is compiled, herdrConformanceSession still gates on requireLiveHerdr so a
// missing binary or the unit-lane env does not fail the suite. Run it via
// `make test-herdr-live` or `go test -tags=integration` with
// GC_HERDR_LIVE_TESTS=1 / GC_FAST_UNIT=0.
func TestHerdrConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return New(herdrConformanceSession(t, &counter), t.TempDir(), t.TempDir(), 0, 0), runtime.Config{WorkDir: t.TempDir()}, fmt.Sprintf("conf-%d", atomic.AddInt64(&counter, 1))
	})
}

// herdrConformanceSession is the ledger-allowed setup helper for the herdr
// proof factory: unique per-city herdr session name, live-tier gate, and
// TeardownServer distinct from Provider.Stop.
func herdrConformanceSession(t *testing.T, counter *int64) string {
	t.Helper()
	requireLiveHerdr(t)
	name := fmt.Sprintf("gctest-conf-%d", atomic.AddInt64(counter, 1))
	t.Cleanup(func() { _ = New(name, "", "", 0, 0).TeardownServer() })
	return name
}
