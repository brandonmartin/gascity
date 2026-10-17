//go:build integration

package tmux

import (
	"context"
	"testing"
)

// TestRestartMarkerOptionRoundTripsOnRealTmux pins the marker's user-option
// mirror against a real tmux server. set-option's -t is a target-pane, and on
// tmux 3.6 a bare "=name" there fails with "no such session", which wrapError
// reports as ErrSessionNotFound, so `gc runtime request-restart` exited 1 on a
// live session. A fake executor cannot see that: only the server resolves the
// target. The session carries the marker in its env (GetMeta) and in the user
// option the batched inventory reads (RuntimeInventory).
func TestRestartMarkerOptionRoundTripsOnRealTmux(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}

	cfg := DefaultConfig()
	cfg.SocketName = privateSocketName("restartmark")
	p := NewProviderWithConfig(cfg)
	const name = "gcrestartmark"
	if err := p.tm.NewSessionWithCommandAndEnv(name, "", "sleep 120", nil); err != nil {
		t.Fatalf("NewSessionWithCommandAndEnv: %v", err)
	}
	t.Cleanup(func() { _ = p.tm.KillSession(name) })

	requested := func() bool {
		t.Helper()
		inv, err := p.RuntimeInventory(context.Background())
		if err != nil {
			t.Fatalf("RuntimeInventory: %v", err)
		}
		entry, ok := inv[name]
		if !ok {
			t.Fatalf("RuntimeInventory has no %q: %v", name, inv)
		}
		return entry.RestartRequested
	}

	if requested() {
		t.Fatal("RestartRequested before any marker was set")
	}
	if err := p.SetMeta(name, "GC_RESTART_REQUESTED", "1700000050"); err != nil {
		t.Fatalf("SetMeta GC_RESTART_REQUESTED on a live session: %v", err)
	}
	if got, err := p.GetMeta(name, "GC_RESTART_REQUESTED"); err != nil || got != "1700000050" {
		t.Fatalf("GetMeta = %q, %v; want the marker the env copy holds", got, err)
	}
	if !requested() {
		t.Fatal("RestartRequested = false after SetMeta: the option mirror was not written")
	}
	if err := p.RemoveMeta(name, "GC_RESTART_REQUESTED"); err != nil {
		t.Fatalf("RemoveMeta GC_RESTART_REQUESTED on a live session: %v", err)
	}
	if requested() {
		t.Fatal("RestartRequested = true after RemoveMeta: the option mirror was not cleared")
	}
}
