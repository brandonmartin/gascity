//go:build integration

package tmux

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// tmuxConformanceConfig returns the production constructor's test config:
// DefaultConfig plus an isolated socket and a short Nudge idle timeout so the
// wait/fallback branch is covered without the production 30s budget.
func tmuxConformanceConfig() Config {
	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.NudgeIdleTimeout = 250 * time.Millisecond
	return cfg
}

// TestTmuxSeamConformance is the ledger-bound runtime.Provider proof for
// NewSeamBackedWithConfig. It does not skip: integration coverage requires tmux,
// and a skip here would leave the production constructor unproved.
func TestTmuxSeamConformance(t *testing.T) {
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithConfig(tmuxConformanceConfig()), runtime.Config{
			Command: "sleep 300",
			WorkDir: t.TempDir(),
		}, fmt.Sprintf("gc-tmux-seam-%d", atomic.AddInt64(&counter, 1))
	})
}
