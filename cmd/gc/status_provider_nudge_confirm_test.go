package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// statusProvider wraps the real session provider on every `gc session nudge`
// (providers.go newSessionProvider). It forwards by field rather than
// embedding, so any provider capability it does not explicitly forward is
// hidden from callers.
//
// That is how the ga-287 strand detection came within one wrapper of being
// inert: the tmux provider reports whether a nudge actually submitted, but a
// wrapper that only forwards Nudge collapses that to "delivered" before the CLI
// ever sees it. These pin the forwarding.

func TestStatusProviderForwardsNudgeConfirmation(t *testing.T) {
	base := runtime.NewFake()
	if err := base.Start(context.Background(), "sess-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	base.SetNudgeSubmitUnconfirmed("sess-1", true)

	wrapped := newBoundedStatusProvider(base)
	cp, ok := wrapped.(runtime.ConfirmingNudgeProvider)
	if !ok {
		t.Fatal("statusProvider does not implement runtime.ConfirmingNudgeProvider; the wrapper hides the base's submit confirmation and strand detection is inert on the CLI path")
	}

	submitted, err := cp.NudgeNowConfirm("sess-1", runtime.TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeNowConfirm: %v", err)
	}
	if submitted {
		t.Fatal("NudgeNowConfirm submitted = true, want the base's unconfirmed submit forwarded through the wrapper")
	}

	submitted, err = cp.NudgeConfirm("sess-1", runtime.TextContent("keep patrolling"))
	if err != nil {
		t.Fatalf("NudgeConfirm: %v", err)
	}
	if submitted {
		t.Fatal("NudgeConfirm submitted = true, want the base's unconfirmed submit forwarded through the wrapper")
	}
}

func TestStatusProviderForwardsConfirmedSubmit(t *testing.T) {
	base := runtime.NewFake()
	if err := base.Start(context.Background(), "sess-2", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cp, ok := newBoundedStatusProvider(base).(runtime.ConfirmingNudgeProvider)
	if !ok {
		t.Fatal("statusProvider does not implement runtime.ConfirmingNudgeProvider")
	}
	submitted, err := cp.NudgeNowConfirm("sess-2", runtime.TextContent("go"))
	if err != nil {
		t.Fatalf("NudgeNowConfirm: %v", err)
	}
	if !submitted {
		t.Fatal("NudgeNowConfirm submitted = false for a healthy submit, want true")
	}
}

// statusProvider previously did not forward NudgeNow at all, which silently
// downgraded every immediate nudge to Nudge's internal wait-idle heuristic.
func TestStatusProviderForwardsImmediateNudge(t *testing.T) {
	base := runtime.NewFake()
	if err := base.Start(context.Background(), "sess-3", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	np, ok := newBoundedStatusProvider(base).(runtime.ImmediateNudgeProvider)
	if !ok {
		t.Fatal("statusProvider does not implement runtime.ImmediateNudgeProvider; immediate nudges silently fall back to the wait-idle path")
	}
	if err := np.NudgeNow("sess-3", runtime.TextContent("go")); err != nil {
		t.Fatalf("NudgeNow: %v", err)
	}
	var sawNudgeNow bool
	for _, call := range base.Calls {
		if call.Method == "NudgeNow" {
			sawNudgeNow = true
		}
	}
	if !sawNudgeNow {
		t.Fatalf("calls = %#v, want the immediate path forwarded to the base", base.Calls)
	}
}
